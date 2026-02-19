/*
Copyright 2025 The Metal3 Authors.

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

package webhooks

import (
	"context"
	"fmt"

	metal3api "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var hostnetworkattachmentlog = logf.Log.WithName("webhooks").WithName("HostNetworkAttachment")

// bmhNetworkAttachmentIndexField is the field index name for BMH -> HostNetworkAttachment references.
const bmhNetworkAttachmentIndexField = ".spec.networkInterfaces.hostNetworkAttachment.name"

func (webhook *HostNetworkAttachment) SetupWebhookWithManager(ctx context.Context, mgr ctrl.Manager) error {
	webhook.Client = mgr.GetClient()

	// Register field indexer for efficient BMH reference lookups
	// This allows us to quickly find all BMHs that reference a specific HostNetworkAttachment
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&metal3api.BareMetalHost{},
		bmhNetworkAttachmentIndexField,
		func(obj client.Object) []string {
			bmh := obj.(*metal3api.BareMetalHost)
			var attachments []string
			for _, iface := range bmh.Spec.NetworkInterfaces {
				if iface.HostNetworkAttachment.Name != "" {
					// Include namespace in index key for cross-namespace support
					ns := iface.HostNetworkAttachment.Namespace
					if ns == "" {
						ns = bmh.Namespace
					}
					key := fmt.Sprintf("%s/%s", ns, iface.HostNetworkAttachment.Name)
					attachments = append(attachments, key)
				}
			}
			return attachments
		},
	); err != nil {
		return err
	}

	return ctrl.NewWebhookManagedBy(mgr).
		For(&metal3api.HostNetworkAttachment{}).
		WithValidator(webhook).
		Complete()
}

//+kubebuilder:webhook:verbs=create;update;delete,path=/validate-metal3-io-v1alpha1-hostnetworkattachment,mutating=false,failurePolicy=fail,sideEffects=none,admissionReviewVersions=v1;v1beta,groups=metal3.io,resources=hostnetworkattachments,versions=v1alpha1,name=hostnetworkattachment.metal3.io

// HostNetworkAttachment implements a validation webhook for HostNetworkAttachment.
type HostNetworkAttachment struct {
	Client client.Client
}

var _ webhook.CustomValidator = &HostNetworkAttachment{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type.
func (webhook *HostNetworkAttachment) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	attachment, ok := obj.(*metal3api.HostNetworkAttachment)
	hostnetworkattachmentlog.Info("validate create", "namespace", attachment.Namespace, "name", attachment.Name)
	if !ok {
		return nil, k8serrors.NewBadRequest(fmt.Sprintf("expected a HostNetworkAttachment but got a %T", obj))
	}
	return nil, kerrors.NewAggregate(webhook.validateAttachment(attachment))
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type.
func (webhook *HostNetworkAttachment) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldAttachment, casted := oldObj.(*metal3api.HostNetworkAttachment)
	if !casted {
		hostnetworkattachmentlog.Error(fmt.Errorf("old object conversion error for %s/%s", oldAttachment.Namespace, oldAttachment.Name), "validate update error")
		return nil, nil
	}

	newAttachment, ok := newObj.(*metal3api.HostNetworkAttachment)
	if !ok {
		return nil, k8serrors.NewBadRequest(fmt.Sprintf("expected a HostNetworkAttachment but got a %T", newObj))
	}

	hostnetworkattachmentlog.Info("validate update", "namespace", newAttachment.Namespace, "name", newAttachment.Name)
	return webhook.validateUpdate(ctx, oldAttachment, newAttachment)
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type.
func (webhook *HostNetworkAttachment) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	attachment, ok := obj.(*metal3api.HostNetworkAttachment)
	if !ok {
		return nil, k8serrors.NewBadRequest(fmt.Sprintf("expected a HostNetworkAttachment but got a %T", obj))
	}

	hostnetworkattachmentlog.Info("validate delete", "namespace", attachment.Namespace, "name", attachment.Name)
	return webhook.validateDelete(ctx, attachment)
}
