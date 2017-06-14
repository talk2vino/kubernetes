/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package webhook delegates admission checks to dynamically configured webhooks.
package webhook

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/golang/glog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/client-go/rest"
	"k8s.io/kubernetes/pkg/api"
	admissionv1alpha1 "k8s.io/kubernetes/pkg/apis/admission/v1alpha1"
	"k8s.io/kubernetes/pkg/apis/admissionregistration"
	admissioninit "k8s.io/kubernetes/pkg/kubeapiserver/admission"

	// install the clientgo admission API for use with api registry
	_ "k8s.io/kubernetes/pkg/apis/admission/install"
)

var (
	groupVersions = []schema.GroupVersion{
		admissionv1alpha1.SchemeGroupVersion,
	}
)

type ErrCallingWebhook struct {
	WebhookName string
	Reason      error
}

func (e *ErrCallingWebhook) Error() string {
	if e.Reason != nil {
		return fmt.Sprintf("failed calling admission webhook %q: %v", e.WebhookName, e.Reason)
	}
	return fmt.Sprintf("failed calling admission webhook %q; no further details available", e.WebhookName)
}

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register("GenericAdmissionWebhook", func(configFile io.Reader) (admission.Interface, error) {
		plugin, err := NewGenericAdmissionWebhook()
		if err != nil {
			return nil, err
		}

		return plugin, nil
	})
}

// NewGenericAdmissionWebhook returns a generic admission webhook plugin.
func NewGenericAdmissionWebhook() (*GenericAdmissionWebhook, error) {
	return &GenericAdmissionWebhook{
		Handler: admission.NewHandler(
			admission.Connect,
			admission.Create,
			admission.Delete,
			admission.Update,
		),
		negotiatedSerializer: serializer.NegotiatedSerializerWrapper(runtime.SerializerInfo{
			Serializer: api.Codecs.LegacyCodec(admissionv1alpha1.SchemeGroupVersion),
		}),
	}, nil
}

// GenericAdmissionWebhook is an implementation of admission.Interface.
type GenericAdmissionWebhook struct {
	*admission.Handler
	hookSource           admissioninit.WebhookSource
	serviceResolver      admissioninit.ServiceResolver
	negotiatedSerializer runtime.NegotiatedSerializer
	clientCert           []byte
	clientKey            []byte
}

var (
	_ = admissioninit.WantsServiceResolver(&GenericAdmissionWebhook{})
	_ = admissioninit.WantsClientCert(&GenericAdmissionWebhook{})
	_ = admissioninit.WantsWebhookSource(&GenericAdmissionWebhook{})
)

func (a *GenericAdmissionWebhook) SetServiceResolver(sr admissioninit.ServiceResolver) {
	a.serviceResolver = sr
}

func (a *GenericAdmissionWebhook) SetClientCert(cert, key []byte) {
	a.clientCert = cert
	a.clientKey = key
}

func (a *GenericAdmissionWebhook) SetWebhookSource(ws admissioninit.WebhookSource) {
	a.hookSource = ws
}

// Admit makes an admission decision based on the request attributes.
func (a *GenericAdmissionWebhook) Admit(attr admission.Attributes) error {
	hooks, err := a.hookSource.List()
	if err != nil {
		return fmt.Errorf("failed listing hooks: %v", err)
	}
	ctx := context.TODO()

	errCh := make(chan error, len(hooks))
	wg := sync.WaitGroup{}
	wg.Add(len(hooks))
	for i := range hooks {
		go func(hook *admissionregistration.ExternalAdmissionHook) {
			defer wg.Done()
			if err := a.callHook(ctx, hook, attr); err == nil {
				return
			} else if callErr, ok := err.(*ErrCallingWebhook); ok {
				glog.Warningf("Failed calling webhook %v: %v", hook.Name, callErr)
				utilruntime.HandleError(callErr)
				// Since we are failing open to begin with, we do not send an error down the channel
			} else {
				glog.Warningf("rejected by webhook %v %t: %v", hook.Name, err, err)
				errCh <- err
			}
		}(&hooks[i])
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	if len(errs) == 0 {
		return nil
	}
	if len(errs) > 1 {
		for i := 1; i < len(errs); i++ {
			// TODO: merge status errors; until then, just return the first one.
			utilruntime.HandleError(errs[i])
		}
	}
	return errs[0]
}

func (a *GenericAdmissionWebhook) callHook(ctx context.Context, h *admissionregistration.ExternalAdmissionHook, attr admission.Attributes) error {
	matches := false
	for _, r := range h.Rules {
		m := RuleMatcher{Rule: r, Attr: attr}
		if m.Matches() {
			matches = true
			break
		}
	}
	if !matches {
		return nil
	}

	// Make the webhook request
	request := admissionv1alpha1.NewAdmissionReview(attr)
	client, err := a.hookClient(h)
	if err != nil {
		return &ErrCallingWebhook{WebhookName: h.Name, Reason: err}
	}
	if err := client.Post().Context(ctx).Body(&request).Do().Into(&request); err != nil {
		return &ErrCallingWebhook{WebhookName: h.Name, Reason: err}
	}

	if request.Status.Allowed {
		return nil
	}

	if request.Status.Result == nil {
		return fmt.Errorf("admission webhook %q denied the request without explanation", h.Name)
	}

	return &apierrors.StatusError{
		ErrStatus: *request.Status.Result,
	}
}

func (a *GenericAdmissionWebhook) hookClient(h *admissionregistration.ExternalAdmissionHook) (*rest.RESTClient, error) {
	u, err := a.serviceResolver.ResolveEndpoint(h.ClientConfig.Service.Namespace, h.ClientConfig.Service.Name)
	if err != nil {
		return nil, err
	}

	// TODO: cache these instead of constructing one each time
	cfg := &rest.Config{
		Host:    u.Host,
		APIPath: u.Path,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   h.ClientConfig.CABundle,
			CertData: a.clientCert,
			KeyData:  a.clientKey,
		},
		UserAgent: "kube-apiserver-admission",
		Timeout:   30 * time.Second,
		ContentConfig: rest.ContentConfig{
			NegotiatedSerializer: a.negotiatedSerializer,
		},
	}
	return rest.UnversionedRESTClientFor(cfg)
}