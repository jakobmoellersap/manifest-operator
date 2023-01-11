package v2

import (
	"context"
	"errors"
	"fmt"

	"github.com/kyma-project/module-manager/internal"
	manifestClient "github.com/kyma-project/module-manager/pkg/client"
	"github.com/kyma-project/module-manager/pkg/types"
	"helm.sh/helm/v3/pkg/kube"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var (
	ErrResourceSyncStateDiff                     = errors.New("resource syncTarget state diff detected")
	ErrInstallationConditionRequiresUpdate       = errors.New("installation condition needs an update")
	ErrDeletionTimestampSetButNotInDeletingState = errors.New("resource is not set to deleting yet")
	ErrObjectHasEmptyState                       = errors.New("object has an empty state")
)

func NewFromManager(mgr manager.Manager, prototype Object, options ...Option) *Reconciler {
	r := &Reconciler{}
	r.prototype = prototype
	r.Options = DefaultOptions().Apply(WithManager(mgr)).Apply(options...)
	return r
}

type Reconciler struct {
	prototype Object
	*Options
}

type ConditionType string

const (
	ConditionTypeResources    ConditionType = "Resources"
	ConditionTypeInstallation ConditionType = "Installation"
)

type ConditionReason string

const (
	ConditionReasonResourcesAreAvailable ConditionReason = "ResourcesAvailable"
	ConditionReasonReady                 ConditionReason = "Ready"
)

func newInstallationCondition(obj Object) metav1.Condition {
	return metav1.Condition{
		Type:               string(ConditionTypeInstallation),
		Reason:             string(ConditionReasonReady),
		Status:             metav1.ConditionFalse,
		Message:            "installation is ready and resources can be used",
		ObservedGeneration: obj.GetGeneration(),
	}
}

func newResourcesCondition(obj Object) metav1.Condition {
	return metav1.Condition{
		Type:               string(ConditionTypeResources),
		Reason:             string(ConditionReasonResourcesAreAvailable),
		Status:             metav1.ConditionFalse,
		Message:            "resources are parsed and ready for use",
		ObservedGeneration: obj.GetGeneration(),
	}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := r.prototype.DeepCopyObject().(Object)
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		log.FromContext(ctx).Info(req.NamespacedName.String() + " got deleted!")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.ShouldSkip(ctx, obj) {
		return ctrl.Result{}, nil
	}

	if err := r.initialize(obj); err != nil {
		return r.ssaStatus(ctx, obj)
	}

	if obj.GetDeletionTimestamp().IsZero() {
		objMeta := r.partialObjectMetadata(obj)
		if controllerutil.AddFinalizer(objMeta, r.Finalizer) {
			return r.ssa(ctx, objMeta)
		}
	}

	spec, err := r.Spec(ctx, obj)
	if err != nil {
		return r.ssaStatus(ctx, obj)
	}

	clnt, err := r.getTargetClient(ctx, obj, spec)
	if err != nil {
		r.Event(obj, "Warning", "ClientInitialization", err.Error())
		obj.SetStatus(obj.GetStatus().WithState(StateError).WithErr(err))
		return r.ssaStatus(ctx, obj)
	}

	converter := NewResourceToInfoConverter(clnt, r.Namespace)

	renderer, err := r.initializeRenderer(ctx, obj, spec, clnt)
	if err != nil {
		return r.ssaStatus(ctx, obj)
	}

	target, current, err := r.renderResources(ctx, obj, renderer, converter)
	if err != nil {
		return r.ssaStatus(ctx, obj)
	}

	diff := kube.ResourceList(current).Difference(target)
	if err := r.pruneDiff(ctx, clnt, obj, renderer, diff); errors.Is(err, ErrDeletionNotFinished) {
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		return r.ssaStatus(ctx, obj)
	}

	if !obj.GetDeletionTimestamp().IsZero() {
		if controllerutil.RemoveFinalizer(obj, r.Finalizer) {
			return ctrl.Result{}, r.Update(ctx, obj) // no SSA since delete does not work for finalizers.
		}
		msg := fmt.Sprintf("waiting as other finalizers are present: %s", obj.GetFinalizers())
		r.Event(obj, "Normal", "FinalizerRemoval", msg)
		obj.SetStatus(obj.GetStatus().WithState(StateDeleting).WithOperation(msg))
		return r.ssaStatus(ctx, obj)
	}

	if err := r.syncResources(ctx, clnt, obj, target); err != nil {
		return r.ssaStatus(ctx, obj)
	}

	return r.CtrlOnSuccess, nil
}

func (r *Reconciler) partialObjectMetadata(obj Object) *metav1.PartialObjectMetadata {
	objMeta := &metav1.PartialObjectMetadata{}
	objMeta.SetName(obj.GetName())
	objMeta.SetNamespace(obj.GetNamespace())
	objMeta.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
	objMeta.SetFinalizers(obj.GetFinalizers())
	return objMeta
}

func (r *Reconciler) initialize(obj Object) error {
	status := obj.GetStatus()

	if !obj.GetDeletionTimestamp().IsZero() && obj.GetStatus().State != StateDeleting {
		obj.SetStatus(status.WithState(StateDeleting).WithErr(ErrDeletionTimestampSetButNotInDeletingState))
		return ErrDeletionTimestampSetButNotInDeletingState
	}

	for _, condition := range []metav1.Condition{
		newResourcesCondition(obj),
		newInstallationCondition(obj),
	} {
		meta.SetStatusCondition(&status.Conditions, condition)
	}

	if status.Synced == nil {
		status.Synced = []Resource{}
	}

	if status.State == "" {
		obj.SetStatus(status.WithState(StateProcessing).WithErr(ErrObjectHasEmptyState))
		return ErrObjectHasEmptyState
	}

	obj.SetStatus(status)

	return nil
}

func (r *Reconciler) Spec(ctx context.Context, obj Object) (*Spec, error) {
	spec, err := r.SpecResolver.Spec(ctx, obj)
	if err != nil {
		r.Event(obj, "Warning", "Spec", err.Error())
		obj.SetStatus(obj.GetStatus().WithState(StateError).WithErr(err))
	}
	return spec, err
}

func (r *Reconciler) renderResources(
	ctx context.Context, obj Object, renderer Renderer, converter ResourceToInfoConverter,
) ([]*resource.Info, []*resource.Info, error) {
	resourceCondition := newResourcesCondition(obj)
	status := obj.GetStatus()

	var err error
	var target, current kube.ResourceList

	if target, err = r.renderTargetResources(ctx, renderer, converter, obj); err != nil {
		return nil, nil, err
	}

	current, err = converter.ResourcesToInfos(status.Synced)
	if err != nil {
		r.Event(obj, "Warning", "CurrentResourceParsing", err.Error())
		obj.SetStatus(status.WithState(StateError).WithErr(err))
		return nil, nil, err
	}

	if !meta.IsStatusConditionTrue(status.Conditions, resourceCondition.Type) {
		r.Event(obj, "Normal", resourceCondition.Reason, resourceCondition.Message)
		resourceCondition.Status = metav1.ConditionTrue
		meta.SetStatusCondition(&status.Conditions, resourceCondition)
		obj.SetStatus(status.WithOperation(resourceCondition.Message))
	}

	return target, current, nil
}

func (r *Reconciler) syncResources(
	ctx context.Context, clnt Client, obj Object, target []*resource.Info,
) error {
	status := obj.GetStatus()

	if err := ConcurrentSSA(clnt, r.FieldOwner).Run(ctx, target); err != nil {
		r.Event(obj, "Warning", "ServerSideApply", err.Error())
		obj.SetStatus(status.WithState(StateError).WithErr(err))
		return err
	}

	oldSynced := status.Synced
	newSynced := NewInfoToResourceConverter().InfosToResources(target)
	status.Synced = newSynced

	if len(ResourcesDiff(oldSynced, newSynced)) > 0 {
		obj.SetStatus(status.WithState(StateProcessing).WithOperation(ErrResourceSyncStateDiff.Error()))
		return ErrResourceSyncStateDiff
	}

	for i := range r.PostRuns {
		if err := r.PostRuns[i](ctx, clnt, r.Client, obj); err != nil {
			r.Event(obj, "Warning", "PostRun", err.Error())
			obj.SetStatus(status.WithState(StateError).WithErr(err))
			return err
		}
	}

	return r.checkTargetReadiness(ctx, clnt, obj, target)
}

func (r *Reconciler) checkTargetReadiness(
	ctx context.Context, clnt Client, obj Object, target []*resource.Info,
) error {
	status := obj.GetStatus()

	resourceReadyCheck := r.CustomReadyCheck
	if resourceReadyCheck == nil {
		resourceReadyCheck = NewHelmReadyCheck(clnt)
	}

	if err := resourceReadyCheck.Run(ctx, target); errors.Is(err, ErrResourcesNotReady) {
		waitingMsg := "waiting for resources to become ready"
		r.Event(obj, "Normal", "ResourceReadyCheck", waitingMsg)
		obj.SetStatus(status.WithState(StateProcessing).WithOperation(waitingMsg))
		return err
	} else if err != nil {
		r.Event(obj, "Warning", "ReadyCheck", err.Error())
		obj.SetStatus(status.WithState(StateError).WithErr(err))
		return err
	}

	installationCondition := newInstallationCondition(obj)
	if !meta.IsStatusConditionTrue(status.Conditions, installationCondition.Type) || status.State != StateReady {
		r.Event(obj, "Normal", installationCondition.Reason, installationCondition.Message)
		installationCondition.Status = metav1.ConditionTrue
		meta.SetStatusCondition(&status.Conditions, installationCondition)
		obj.SetStatus(status.WithState(StateReady).WithOperation(installationCondition.Message))
		return ErrInstallationConditionRequiresUpdate
	}

	return nil
}

func (r *Reconciler) deleteResources(
	ctx context.Context, clnt Client, obj Object, diff []*resource.Info,
) error {
	status := obj.GetStatus()

	if !obj.GetDeletionTimestamp().IsZero() {
		for _, preDelete := range r.PreDeletes {
			if err := preDelete(ctx, clnt, r.Client, obj); err != nil {
				r.Event(obj, "Warning", "PreDelete", err.Error())
				// we do not set a status here since it will be deleting if timestamp is set to zero.
				return err
			}
		}
	}

	if err := NewConcurrentCleanup(clnt).Run(ctx, diff); errors.Is(err, ErrDeletionNotFinished) {
		r.Event(obj, "Normal", "Deletion", err.Error())
		return err
	} else if err != nil {
		r.Event(obj, "Warning", "Deletion", err.Error())
		obj.SetStatus(status.WithState(StateError).WithErr(err))
		return err
	}

	return nil
}

func (r *Reconciler) renderTargetResources(
	ctx context.Context, renderer Renderer, converter ResourceToInfoConverter, obj Object,
) ([]*resource.Info, error) {
	if !obj.GetDeletionTimestamp().IsZero() {
		// if we are deleting the resources,
		// we no longer want to have any target resources and want to clean up all existing resources.
		// Thus, we empty the target here so the difference will be the entire current
		// resource list in the cluster.
		return kube.ResourceList{}, nil
	}

	status := obj.GetStatus()

	manifest, err := renderer.Render(ctx, obj)
	if err != nil {
		return nil, err
	}

	targetResources, err := internal.ParseManifestStringToObjects(string(manifest))
	if err != nil {
		r.Event(obj, "Warning", "ManifestParsing", err.Error())
		obj.SetStatus(status.WithState(StateError).WithErr(err))
		return nil, err
	}

	for _, transform := range r.PostRenderTransforms {
		if err := transform(ctx, obj, targetResources.Items); err != nil {
			r.Event(obj, "Warning", "PostRenderTransform", err.Error())
			obj.SetStatus(status.WithState(StateError).WithErr(err))
			return nil, err
		}
	}

	target, err := converter.UnstructuredToInfos(targetResources.Items)
	if err != nil {
		r.Event(obj, "Warning", "TargetResourceParsing", err.Error())
		obj.SetStatus(status.WithState(StateError).WithErr(err))
		return nil, err
	}

	return target, nil
}

func (r *Reconciler) initializeRenderer(ctx context.Context, obj Object, spec *Spec, client Client) (Renderer, error) {
	var renderer Renderer

	switch spec.Mode {
	case RenderModeHelm:
		renderer = NewHelmRenderer(spec, client, r.Options)
		renderer = WrapWithRendererCache(renderer, spec, r.Options)
	case RenderModeKustomize:
		renderer = NewKustomizeRenderer(spec, r.Options)
		renderer = WrapWithRendererCache(renderer, spec, r.Options)
	case RenderModeRaw:
		renderer = NewRawRenderer(spec, r.Options)
	}

	if err := renderer.Initialize(obj); err != nil {
		return nil, err
	}
	if err := renderer.EnsurePrerequisites(ctx, obj); err != nil {
		return nil, err
	}

	return renderer, nil
}

func (r *Reconciler) pruneDiff(
	ctx context.Context, clnt Client, obj Object, renderer Renderer, diff []*resource.Info,
) error {
	if err := r.deleteResources(ctx, clnt, obj, diff); err != nil {
		return err
	}

	if obj.GetDeletionTimestamp().IsZero() || !r.DeletePrerequisites {
		return nil
	}

	return renderer.RemovePrerequisites(ctx, obj)
}

func (r *Reconciler) getTargetClient(
	ctx context.Context, obj Object, spec *Spec,
) (Client, error) {
	var err error

	clientsCacheKey := r.ClientCacheKeyFn(ctx, obj)

	clnt := r.GetClientFromCache(clientsCacheKey)

	if clnt == nil {
		cluster := &types.ClusterInfo{
			Config: r.Config,
			Client: r.Client,
		}
		if r.TargetCluster != nil {
			cluster, err = r.TargetCluster(ctx, obj)
		}
		if err != nil {
			return nil, err
		}
		clnt, err = manifestClient.NewSingletonClients(cluster, log.FromContext(ctx))
		if err != nil {
			return nil, err
		}
		clnt.Install().Atomic = false
		clnt.Install().Replace = true
		clnt.Install().DryRun = true
		clnt.Install().IncludeCRDs = false
		clnt.Install().CreateNamespace = r.CreateNamespace
		clnt.Install().UseReleaseName = false
		clnt.Install().IsUpgrade = true
		clnt.Install().DisableHooks = true
		clnt.Install().DisableOpenAPIValidation = true
		if clnt.Install().Version == "" && clnt.Install().Devel {
			clnt.Install().Version = ">0.0.0-0"
		}
		clnt.Install().ReleaseName = spec.ManifestName
		r.SetClientInCache(clientsCacheKey, clnt)
	}

	clnt.Install().Namespace = r.Namespace
	clnt.KubeClient().Namespace = r.Namespace

	if r.Namespace != metav1.NamespaceNone && r.Namespace != metav1.NamespaceDefault &&
		clnt.Install().CreateNamespace {
		err := clnt.Patch(
			ctx, &v1.Namespace{
				TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: r.Namespace},
			}, client.Apply, client.ForceOwnership, r.FieldOwner,
		)
		if err != nil {
			return nil, err
		}
	}

	return clnt, nil
}

func (r *Reconciler) ssaStatus(ctx context.Context, obj client.Object) (ctrl.Result, error) {
	obj.SetUID("")
	obj.SetManagedFields(nil)
	obj.SetResourceVersion("")
	// TODO: replace the SubResourcePatchOptions with  client.ForceOwnership, r.FieldOwner in later compatible version
	return ctrl.Result{Requeue: true}, r.Status().Patch(
		ctx, obj, client.Apply, subResourceOpts(client.ForceOwnership, r.FieldOwner),
	)
}

func subResourceOpts(opts ...client.PatchOption) client.SubResourcePatchOption {
	return &client.SubResourcePatchOptions{PatchOptions: *(&client.PatchOptions{}).ApplyOptions(opts)}
}

func (r *Reconciler) ssa(ctx context.Context, obj client.Object) (ctrl.Result, error) {
	obj.SetUID("")
	obj.SetManagedFields(nil)
	obj.SetResourceVersion("")
	return ctrl.Result{Requeue: true}, r.Patch(ctx, obj, client.Apply, client.ForceOwnership, r.FieldOwner)
}
