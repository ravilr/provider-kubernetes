/*
Copyright 2021 The Crossplane Authors.

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

package object

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/feature"
	"github.com/crossplane/crossplane-runtime/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane-contrib/provider-kubernetes/apis/object/v1alpha2"
	apisv1alpha1 "github.com/crossplane-contrib/provider-kubernetes/apis/v1alpha1"
	"github.com/crossplane-contrib/provider-kubernetes/internal/clients"
	"github.com/crossplane-contrib/provider-kubernetes/internal/clients/azure"
	"github.com/crossplane-contrib/provider-kubernetes/internal/clients/gke"
)

const (
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCreds     = "cannot get credentials"
	errGetObject    = "cannot get object"
	errCreateObject = "cannot create object"
	errApplyObject  = "cannot apply object"
	errDeleteObject = "cannot delete object"

	errNotKubernetesObject              = "managed resource is not an Object custom resource"
	errNewKubernetesClient              = "cannot create new Kubernetes client"
	errFailedToCreateRestConfig         = "cannot create new REST config using provider secret"
	errFailedToExtractGoogleCredentials = "cannot extract Google Application Credentials"
	errFailedToInjectGoogleCredentials  = "cannot wrap REST client with Google Application Credentials"
	errFailedToExtractAzureCredentials  = "failed to extract Azure Application Credentials"
	errFailedToInjectAzureCredentials   = "failed to wrap REST client with Azure Application Credentials"

	errGetLastApplied          = "cannot get last applied"
	errUnmarshalTemplate       = "cannot unmarshal template"
	errFailedToMarshalExisting = "cannot marshal existing resource"

	errGetReferencedResource       = "cannot get referenced resource"
	errPatchFromReferencedResource = "cannot patch from referenced resource"
	errResolveResourceReferences   = "cannot resolve resource references"

	errAddFinalizer             = "cannot add finalizer to Object"
	errRemoveFinalizer          = "cannot remove finalizer from Object"
	errAddReferenceFinalizer    = "cannot add finalizer to referenced resource"
	errRemoveReferenceFinalizer = "cannot remove finalizer from referenced resource"
	objFinalizerName            = "finalizer.managedresource.crossplane.io"
	refFinalizerNamePrefix      = "kubernetes.crossplane.io/referred-by-object-"

	errGetConnectionDetails = "cannot get connection details"
	errGetValueAtFieldPath  = "cannot get value at fieldPath"
	errDecodeSecretData     = "cannot decode secret data"
	errSanitizeSecretData   = "cannot sanitize secret data"
)

// Setup adds a controller that reconciles Object managed resources.
func Setup(mgr ctrl.Manager, o controller.Options, sanitizeSecrets bool) error {
	name := managed.ControllerName(v1alpha2.ObjectGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}

	reconcilerOptions := []managed.ReconcilerOption{
		managed.WithExternalConnecter(&connector{
			logger:           o.Logger,
			sanitizeSecrets:  sanitizeSecrets,
			kube:             mgr.GetClient(),
			usage:            resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			kcfgExtractorFn:  resource.CommonCredentialExtractor,
			gcpExtractorFn:   resource.CommonCredentialExtractor,
			gcpInjectorFn:    gke.WrapRESTConfig,
			azureExtractorFn: resource.CommonCredentialExtractor,
			azureInjectorFn:  azure.WrapRESTConfig,
			newRESTConfigFn:  clients.NewRESTConfig,
			newKubeClientFn:  clients.NewKubeClient,
		}),
		managed.WithFinalizer(&objFinalizer{client: mgr.GetClient()}),
		managed.WithPollInterval(o.PollInterval),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...),
	}

	if o.Features.Enabled(feature.EnableBetaManagementPolicies) {
		reconcilerOptions = append(reconcilerOptions, managed.WithManagementPolicies())
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha2.ObjectGroupVersionKind),
		reconcilerOptions...,
	)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha2.Object{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

type connector struct {
	kube            client.Client
	usage           resource.Tracker
	logger          logging.Logger
	sanitizeSecrets bool

	kcfgExtractorFn  func(ctx context.Context, src xpv1.CredentialsSource, c client.Client, ccs xpv1.CommonCredentialSelectors) ([]byte, error)
	gcpExtractorFn   func(ctx context.Context, src xpv1.CredentialsSource, c client.Client, ccs xpv1.CommonCredentialSelectors) ([]byte, error)
	gcpInjectorFn    func(ctx context.Context, rc *rest.Config, credentials []byte, scopes ...string) error
	azureExtractorFn func(ctx context.Context, src xpv1.CredentialsSource, c client.Client, ccs xpv1.CommonCredentialSelectors) ([]byte, error)
	azureInjectorFn  func(ctx context.Context, rc *rest.Config, credentials []byte, scopes ...string) error
	newRESTConfigFn  func(kubeconfig []byte) (*rest.Config, error)
	newKubeClientFn  func(config *rest.Config) (client.Client, error)
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) { //nolint:gocyclo
	// This method is currently a little over our complexity goal - be wary
	// of making it more complex.

	cr, ok := mg.(*v1alpha2.Object)
	if !ok {
		return nil, errors.New(errNotKubernetesObject)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	var rc *rest.Config
	var err error

	switch cd := pc.Spec.Credentials; cd.Source { //nolint:exhaustive
	case xpv1.CredentialsSourceInjectedIdentity:
		rc, err = rest.InClusterConfig()
		if err != nil {
			return nil, errors.Wrap(err, errFailedToCreateRestConfig)
		}
	default:
		kc, err := c.kcfgExtractorFn(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
		if err != nil {
			return nil, errors.Wrap(err, errGetCreds)
		}

		if rc, err = c.newRESTConfigFn(kc); err != nil {
			return nil, errors.Wrap(err, errFailedToCreateRestConfig)
		}
	}

	if id := pc.Spec.Identity; id != nil {
		switch id.Type {
		case apisv1alpha1.IdentityTypeGoogleApplicationCredentials:
			switch id.Source { //nolint:exhaustive
			case xpv1.CredentialsSourceInjectedIdentity:
				if err := c.gcpInjectorFn(ctx, rc, nil, gke.DefaultScopes...); err != nil {
					return nil, errors.Wrap(err, errFailedToInjectGoogleCredentials)
				}
			default:
				creds, err := c.gcpExtractorFn(ctx, id.Source, c.kube, id.CommonCredentialSelectors)
				if err != nil {
					return nil, errors.Wrap(err, errFailedToExtractGoogleCredentials)
				}

				if err := c.gcpInjectorFn(ctx, rc, creds, gke.DefaultScopes...); err != nil {
					return nil, errors.Wrap(err, errFailedToInjectGoogleCredentials)
				}
			}
		case apisv1alpha1.IdentityTypeAzureServicePrincipalCredentials:
			switch id.Source { //nolint:exhaustive
			case xpv1.CredentialsSourceInjectedIdentity:
				return nil, errors.Errorf("%s is not supported as identity source for identity type %s",
					xpv1.CredentialsSourceInjectedIdentity, apisv1alpha1.IdentityTypeAzureServicePrincipalCredentials)
			default:
				creds, err := c.azureExtractorFn(ctx, id.Source, c.kube, id.CommonCredentialSelectors)
				if err != nil {
					return nil, errors.Wrap(err, errFailedToExtractAzureCredentials)
				}

				if err := c.azureInjectorFn(ctx, rc, creds); err != nil {
					return nil, errors.Wrap(err, errFailedToInjectAzureCredentials)
				}
			}
		default:
			return nil, errors.Errorf("unknown identity type: %s", id.Type)
		}
	}

	k, err := c.newKubeClientFn(rc)
	if err != nil {
		return nil, errors.Wrap(err, errNewKubernetesClient)
	}

	return &external{
		logger: c.logger,
		client: resource.ClientApplicator{
			Client:     k,
			Applicator: resource.NewAPIPatchingApplicator(k),
		},
		localClient:     c.kube,
		sanitizeSecrets: c.sanitizeSecrets,
	}, nil
}

type external struct {
	logger logging.Logger
	client resource.ClientApplicator
	// localClient is specifically used to connect to local cluster
	localClient     client.Client
	sanitizeSecrets bool
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha2.Object)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotKubernetesObject)
	}

	c.logger.Debug("Observing", "resource", cr)

	if err := c.resolveReferencies(ctx, cr); err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, errResolveResourceReferences)
	}

	desired, err := getDesired(cr)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	observed := desired.DeepCopy()

	err = c.client.Get(ctx, types.NamespacedName{
		Namespace: observed.GetNamespace(),
		Name:      observed.GetName(),
	}, observed)

	if kerrors.IsNotFound(err) {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, errGetObject)
	}

	if err = c.setObserved(cr, observed); err != nil {
		return managed.ExternalObservation{}, err
	}

	var last *unstructured.Unstructured
	if last, err = getLastApplied(cr, observed); err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, errGetLastApplied)
	}
	return c.handleLastApplied(ctx, cr, last, desired)
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha2.Object)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotKubernetesObject)
	}

	c.logger.Debug("Creating", "resource", cr)

	obj, err := getDesired(cr)
	if err != nil {
		return managed.ExternalCreation{}, err
	}

	meta.AddAnnotations(obj, map[string]string{
		v1.LastAppliedConfigAnnotation: string(cr.Spec.ForProvider.Manifest.Raw),
	})

	if err := c.client.Create(ctx, obj); err != nil {
		return managed.ExternalCreation{}, errors.Wrap(err, errCreateObject)
	}

	return managed.ExternalCreation{}, c.setObserved(cr, obj)
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha2.Object)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotKubernetesObject)
	}

	c.logger.Debug("Updating", "resource", cr)

	obj, err := getDesired(cr)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}

	meta.AddAnnotations(obj, map[string]string{
		v1.LastAppliedConfigAnnotation: string(cr.Spec.ForProvider.Manifest.Raw),
	})

	if err := c.client.Apply(ctx, obj); err != nil {
		return managed.ExternalUpdate{}, errors.Wrap(CleanErr(err), errApplyObject)
	}

	return managed.ExternalUpdate{}, c.setObserved(cr, obj)
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha2.Object)
	if !ok {
		return errors.New(errNotKubernetesObject)
	}

	c.logger.Debug("Deleting", "resource", cr)

	obj, err := getDesired(cr)
	if err != nil {
		return err
	}

	return errors.Wrap(resource.IgnoreNotFound(c.client.Delete(ctx, obj)), errDeleteObject)
}

func getDesired(obj *v1alpha2.Object) (*unstructured.Unstructured, error) {
	desired := &unstructured.Unstructured{}
	if err := json.Unmarshal(obj.Spec.ForProvider.Manifest.Raw, desired); err != nil {
		return nil, errors.Wrap(err, errUnmarshalTemplate)
	}

	if desired.GetName() == "" {
		desired.SetName(obj.Name)
	}
	return desired, nil
}

func getLastApplied(obj *v1alpha2.Object, observed *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	lastApplied, ok := observed.GetAnnotations()[v1.LastAppliedConfigAnnotation]
	if !ok {
		return nil, nil
	}

	last := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(lastApplied), last); err != nil {
		return nil, errors.Wrap(err, errUnmarshalTemplate)
	}

	if last.GetName() == "" {
		last.SetName(obj.Name)
	}

	return last, nil
}

func (c *external) setObserved(obj *v1alpha2.Object, observed *unstructured.Unstructured) error {
	var err error

	if c.sanitizeSecrets {
		if observed.GetKind() == "Secret" && observed.GetAPIVersion() == "v1" {
			data := map[string][]byte{"redacted": []byte(nil)}
			if err = fieldpath.Pave(observed.Object).SetValue("data", data); err != nil {
				return errors.Wrap(err, errSanitizeSecretData)
			}
		}
	}

	if obj.Status.AtProvider.Manifest.Raw, err = observed.MarshalJSON(); err != nil {
		return errors.Wrap(err, errFailedToMarshalExisting)
	}

	if err := c.updateConditionFromObserved(obj, observed); err != nil {
		return err
	}
	return nil
}

func (c *external) updateConditionFromObserved(obj *v1alpha2.Object, observed *unstructured.Unstructured) error {
	switch obj.Spec.Readiness.Policy {
	case v1alpha2.ReadinessPolicyDeriveFromObject:
		conditioned := xpv1.ConditionedStatus{}
		err := fieldpath.Pave(observed.Object).GetValueInto("status", &conditioned)
		if err != nil {
			c.logger.Debug("Got error while getting conditions from observed object, setting it as Unavailable", "error", err, "observed", observed)
			obj.SetConditions(xpv1.Unavailable())
			return nil
		}
		if status := conditioned.GetCondition(xpv1.TypeReady).Status; status != v1.ConditionTrue {
			c.logger.Debug("Observed object is not ready, setting it as Unavailable", "status", status, "observed", observed)
			obj.SetConditions(xpv1.Unavailable())
			return nil
		}
		obj.SetConditions(xpv1.Available())
	case v1alpha2.ReadinessPolicyAllTrue:
		conditioned := xpv1.ConditionedStatus{}
		err := fieldpath.Pave(observed.Object).GetValueInto("status", &conditioned)
		if err != nil {
			c.logger.Debug("Got error while getting conditions from observed object, setting it as Unavailable", "error", err, "observed", observed)
			obj.SetConditions(xpv1.Unavailable())
			return nil
		}
		allTrue := len(conditioned.Conditions) > 0
		for _, condition := range conditioned.Conditions {
			if condition.Status != v1.ConditionTrue {
				allTrue = false
				break
			}
		}
		if allTrue {
			obj.SetConditions(xpv1.Available())
		} else {
			obj.SetConditions(xpv1.Unavailable())
		}
	case v1alpha2.ReadinessPolicySuccessfulCreate, "":
		// do nothing, will be handled by c.handleLastApplied method
		// "" should never happen, but just in case we will treat it as SuccessfulCreate for backward compatibility
	default:
		// should never happen
		return errors.Errorf("unknown readiness policy %q", obj.Spec.Readiness.Policy)
	}
	return nil
}

func getReferenceInfo(ref v1alpha2.Reference) (string, string, string, string) {
	var apiVersion, kind, namespace, name string

	if ref.PatchesFrom != nil {
		// Reference information defined in PatchesFrom
		apiVersion = ref.PatchesFrom.APIVersion
		kind = ref.PatchesFrom.Kind
		namespace = ref.PatchesFrom.Namespace
		name = ref.PatchesFrom.Name
	} else if ref.DependsOn != nil {
		// Reference information defined in DependsOn
		apiVersion = ref.DependsOn.APIVersion
		kind = ref.DependsOn.Kind
		namespace = ref.DependsOn.Namespace
		name = ref.DependsOn.Name
	}

	return apiVersion, kind, namespace, name
}

// resolveReferencies resolves references for the current Object. If it fails to
// resolve some reference, e.g.: due to reference not ready, it will then return
// error and requeue to wait for resolving it next time.
func (c *external) resolveReferencies(ctx context.Context, obj *v1alpha2.Object) error {
	c.logger.Debug("Resolving referencies.")

	// Loop through references to resolve each referenced resource
	for _, ref := range obj.Spec.References {
		if ref.DependsOn == nil && ref.PatchesFrom == nil {
			continue
		}

		refAPIVersion, refKind, refNamespace, refName := getReferenceInfo(ref)
		res := &unstructured.Unstructured{}
		res.SetAPIVersion(refAPIVersion)
		res.SetKind(refKind)
		// Try to get referenced resource
		err := c.localClient.Get(ctx, client.ObjectKey{
			Namespace: refNamespace,
			Name:      refName,
		}, res)

		if err != nil {
			return errors.Wrap(err, errGetReferencedResource)
		}

		// Patch fields if any
		if ref.PatchesFrom != nil && ref.PatchesFrom.FieldPath != nil {
			if err := ref.ApplyFromFieldPathPatch(res, obj); err != nil {
				return errors.Wrap(err, errPatchFromReferencedResource)
			}
		}
	}

	return nil
}

func (c *external) handleLastApplied(ctx context.Context, obj *v1alpha2.Object, last, desired *unstructured.Unstructured) (managed.ExternalObservation, error) {
	isUpToDate := false

	if !sets.New[xpv1.ManagementAction](obj.GetManagementPolicies()...).
		HasAny(xpv1.ManagementActionUpdate, xpv1.ManagementActionCreate, xpv1.ManagementActionAll) {
		// Treated as up-to-date as we don't update or create the resource
		isUpToDate = true
	}
	if last != nil && equality.Semantic.DeepEqual(last, desired) {
		// Mark as up-to-date since last is equal to desired
		isUpToDate = true
	}

	if isUpToDate {
		c.logger.Debug("Up to date!")

		if p := obj.Spec.Readiness.Policy; p == v1alpha2.ReadinessPolicySuccessfulCreate || p == "" {
			obj.Status.SetConditions(xpv1.Available())
		}

		cd, err := connectionDetails(ctx, c.client, obj.Spec.ConnectionDetails)
		if err != nil {
			return managed.ExternalObservation{}, errors.Wrap(err, errGetConnectionDetails)
		}

		return managed.ExternalObservation{
			ResourceExists:    true,
			ResourceUpToDate:  true,
			ConnectionDetails: cd,
		}, nil
	}

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: false,
	}, nil
}

type objFinalizer struct {
	resource.Finalizer
	client client.Client
}

type refFinalizerFn func(context.Context, *unstructured.Unstructured, string) error

func (f *objFinalizer) handleRefFinalizer(ctx context.Context, obj *v1alpha2.Object, finalizerFn refFinalizerFn, ignoreNotFound bool) error {
	// Loop through references to resolve each referenced resource
	for _, ref := range obj.Spec.References {
		if ref.DependsOn == nil && ref.PatchesFrom == nil {
			continue
		}

		refAPIVersion, refKind, refNamespace, refName := getReferenceInfo(ref)
		res := &unstructured.Unstructured{}
		res.SetAPIVersion(refAPIVersion)
		res.SetKind(refKind)
		// Try to get referenced resource
		err := f.client.Get(ctx, client.ObjectKey{
			Namespace: refNamespace,
			Name:      refName,
		}, res)

		if err != nil {
			if ignoreNotFound && kerrors.IsNotFound(err) {
				continue
			}

			return errors.Wrap(err, errGetReferencedResource)
		}

		finalizerName := refFinalizerNamePrefix + string(obj.UID)
		if err = finalizerFn(ctx, res, finalizerName); err != nil {
			return err
		}
	}

	return nil

}

func (f *objFinalizer) AddFinalizer(ctx context.Context, res resource.Object) error {
	obj, ok := res.(*v1alpha2.Object)
	if !ok {
		return errors.New(errNotKubernetesObject)
	}

	if meta.FinalizerExists(obj, objFinalizerName) {
		return nil
	}
	meta.AddFinalizer(obj, objFinalizerName)

	err := f.client.Update(ctx, obj)
	if err != nil {
		return errors.Wrap(err, errAddFinalizer)
	}

	// Add finalizer to referenced resources if not exists
	err = f.handleRefFinalizer(ctx, obj, func(
		ctx context.Context, res *unstructured.Unstructured, finalizer string) error {
		if !meta.FinalizerExists(res, finalizer) {
			meta.AddFinalizer(res, finalizer)
			if err := f.client.Update(ctx, res); err != nil {
				return errors.Wrap(err, errAddReferenceFinalizer)
			}
		}
		return nil
	}, false)
	return errors.Wrap(err, errAddFinalizer)
}

func (f *objFinalizer) RemoveFinalizer(ctx context.Context, res resource.Object) error {
	obj, ok := res.(*v1alpha2.Object)
	if !ok {
		return errors.New(errNotKubernetesObject)
	}

	// Remove finalizer from referenced resources if exists
	err := f.handleRefFinalizer(ctx, obj, func(
		ctx context.Context, res *unstructured.Unstructured, finalizer string) error {
		if meta.FinalizerExists(res, finalizer) {
			meta.RemoveFinalizer(res, finalizer)
			if err := f.client.Update(ctx, res); err != nil {
				return errors.Wrap(err, errRemoveReferenceFinalizer)
			}
		}
		return nil
	}, true)
	if err != nil {
		return errors.Wrap(err, errRemoveFinalizer)
	}

	if !meta.FinalizerExists(obj, objFinalizerName) {
		return nil
	}
	meta.RemoveFinalizer(obj, objFinalizerName)

	err = f.client.Update(ctx, obj)
	return errors.Wrap(err, errRemoveFinalizer)
}

func connectionDetails(ctx context.Context, kube client.Client, connDetails []v1alpha2.ConnectionDetail) (managed.ConnectionDetails, error) {
	mcd := managed.ConnectionDetails{}

	for _, cd := range connDetails {
		ro := unstructuredFromObjectRef(cd.ObjectReference)
		if err := kube.Get(ctx, types.NamespacedName{Name: ro.GetName(), Namespace: ro.GetNamespace()}, &ro); err != nil {
			return mcd, errors.Wrap(err, errGetObject)
		}

		paved := fieldpath.Pave(ro.Object)
		v, err := paved.GetValue(cd.FieldPath)
		if err != nil {
			return mcd, errors.Wrap(err, errGetValueAtFieldPath)
		}

		s := fmt.Sprintf("%v", v)
		fv := []byte(s)
		// prevent secret data being encoded twice
		if cd.Kind == "Secret" && cd.APIVersion == "v1" && strings.HasPrefix(cd.FieldPath, "data") {
			fv, err = base64.StdEncoding.DecodeString(s)
			if err != nil {
				return mcd, errors.Wrap(err, errDecodeSecretData)
			}
		}

		mcd[cd.ToConnectionSecretKey] = fv
	}

	return mcd, nil
}

func unstructuredFromObjectRef(r v1.ObjectReference) unstructured.Unstructured {
	u := unstructured.Unstructured{}
	u.SetAPIVersion(r.APIVersion)
	u.SetKind(r.Kind)
	u.SetName(r.Name)
	u.SetNamespace(r.Namespace)

	return u
}
