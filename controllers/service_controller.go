/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"

	"github.com/aws/aws-application-networking-k8s/controllers/eventhandlers"
	"github.com/aws/aws-application-networking-k8s/pkg/k8s"
	"github.com/aws/aws-application-networking-k8s/pkg/utils/gwlog"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gateway_api "sigs.k8s.io/gateway-api/apis/v1beta1"
	mcs_api "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"

	"github.com/aws/aws-application-networking-k8s/pkg/aws"
	"github.com/aws/aws-application-networking-k8s/pkg/deploy"
	"github.com/aws/aws-application-networking-k8s/pkg/gateway"
	"github.com/aws/aws-application-networking-k8s/pkg/latticestore"
	"github.com/aws/aws-application-networking-k8s/pkg/model/core"
	latticemodel "github.com/aws/aws-application-networking-k8s/pkg/model/lattice"
)

const (
	// Typo
	serviceFinalizer = "service.ki8s.aws/resources"
)

// serviceReconciler reconciles a Service object
type serviceReconciler struct {
	log              gwlog.Logger
	client           client.Client
	scheme           *runtime.Scheme
	finalizerManager k8s.FinalizerManager
	eventRecorder    record.EventRecorder
	modelBuilder     gateway.LatticeTargetsBuilder
	stackDeployer    deploy.StackDeployer
	datastore        *latticestore.LatticeDataStore
	stackMashaller   deploy.StackMarshaller
}

func RegisterServiceController(
	log gwlog.Logger,
	cloud aws.Cloud,
	datastore *latticestore.LatticeDataStore,
	finalizerManager k8s.FinalizerManager,
	mgr ctrl.Manager,
) error {
	client := mgr.GetClient()
	scheme := mgr.GetScheme()
	evtRec := mgr.GetEventRecorderFor("service")
	modelBuild := gateway.NewTargetsBuilder(client, cloud, datastore)
	stackDeploy := deploy.NewTargetsStackDeploy(cloud, client, datastore)
	stackMarshaller := deploy.NewDefaultStackMarshaller()
	sr := &serviceReconciler{
		log:              log,
		client:           client,
		scheme:           scheme,
		finalizerManager: finalizerManager,
		modelBuilder:     modelBuild,
		stackDeployer:    stackDeploy,
		eventRecorder:    evtRec,
		datastore:        datastore,
		stackMashaller:   stackMarshaller,
	}
	epsEventsHandler := eventhandlers.NewEnqueueRequestEndpointEvent(client)
	httpRouteEventHandler := eventhandlers.NewEnqueueRequestHTTPRouteEvent(client)
	serviceExportHandler := eventhandlers.NewEqueueRequestServiceExportEvent(client)
	err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&source.Kind{Type: &corev1.Endpoints{}}, epsEventsHandler).
		Watches(&source.Kind{Type: &gateway_api.HTTPRoute{}}, httpRouteEventHandler).
		Watches(&source.Kind{Type: &mcs_api.ServiceExport{}}, serviceExportHandler).
		Complete(sr)
	return err
}

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=endpoints/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=configmaps, verbs=create;delete;patch;update;get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (r *serviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.log.Infow("reconcile", "name", req.Name)

	svc := &corev1.Service{}
	if err := r.client.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tgName := latticestore.TargetGroupName(svc.Name, svc.Namespace)
	tgs := r.datastore.GetTargetGroupsByName(tgName)
	if !svc.DeletionTimestamp.IsZero() {
		for _, tg := range tgs {
			r.log.Debugf("deletion request for tgName: %s: at timestamp: %s", tg.TargetGroupKey.Name, svc.DeletionTimestamp)
			r.reconcileTargetsResource(ctx, svc, tg.TargetGroupKey.RouteName)
		}
		r.finalizerManager.RemoveFinalizers(ctx, svc, serviceFinalizer)
	} else {
		// TODO also need to check serviceexport object to trigger building TargetGroup
		for _, tg := range tgs {
			r.log.Debugf("update request for tgName: %s", tg.TargetGroupKey.Name)
			r.reconcileTargetsResource(ctx, svc, tg.TargetGroupKey.RouteName)
		}
	}
	r.log.Infow("reconciled", "name", req.Name)
	return ctrl.Result{}, nil
}

func (r *serviceReconciler) reconcileTargetsResource(ctx context.Context, svc *corev1.Service, routename string) {
	if err := r.finalizerManager.AddFinalizers(ctx, svc, serviceFinalizer); err != nil {
		r.eventRecorder.Event(svc, corev1.EventTypeWarning, k8s.ServiceEventReasonFailedAddFinalizer, fmt.Sprintf("failed and finalizer: %s", err))
	}
	r.buildAndDeployModel(ctx, svc, routename)
}

func (r *serviceReconciler) buildAndDeployModel(ctx context.Context, svc *corev1.Service, routename string) (core.Stack, *latticemodel.Targets, error) {
	stack, latticeTargets, err := r.modelBuilder.Build(ctx, svc, routename)
	if err != nil {
		r.eventRecorder.Event(svc, corev1.EventTypeWarning,
			k8s.ServiceEventReasonFailedBuildModel, fmt.Sprintf("failed build model: %s", err))
		return nil, nil, err
	}

	jsonStack, _ := r.stackMashaller.Marshal(stack)
	r.log.Debugw("successfully built model", "stack", jsonStack)

	if err := r.stackDeployer.Deploy(ctx, stack); err != nil {
		r.eventRecorder.Event(svc, corev1.EventTypeWarning,
			k8s.ServiceEventReasonFailedDeployModel, fmt.Sprintf("failed deploy model: %s", err))
		return nil, nil, err
	}

	r.log.Debugw("successfully deployed model", "service", svc.Name)
	return stack, latticeTargets, err
}
