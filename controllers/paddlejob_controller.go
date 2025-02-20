// Copyright 2021 The Kubeflow Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pdv1 "github.com/paddleflow/paddle-operator/api/v1"
)

const (
	HOST_PORT_START = "port.start"
	HOST_PORT_END   = "port.end"
	HOST_PORT_CUR   = "port.cur"
	HOST_PORT_NUM   = 20
	PADDLE_PORT     = 2379
)

var (
	ctrlRefKey    = ".metadata.controller"
	apiGVStr      = pdv1.GroupVersion.String()
	finalizerName = "finalizers.paddlepaddle.org"
	hostPort      = "host-port"
)

// PaddleJobReconciler reconciles a PaddleJob object
type PaddleJobReconciler struct {
	client.Client
	Log         logr.Logger
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	HostPortMap map[string]int
}

//+kubebuilder:rbac:groups=batch.paddlepaddle.org,resources=paddlejobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.paddlepaddle.org,resources=paddlejobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.paddlepaddle.org,resources=paddlejobs/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods/status,verbs=get
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services/status,verbs=get
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Reconcile function compares the state specified by
// the PaddleJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *PaddleJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("paddlejob", req.NamespacedName)

	// Obtain the PaddleJob instance we are working on
	var pdj pdv1.PaddleJob
	if err := r.Get(ctx, req.NamespacedName, &pdj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconcile", "version", pdj.ResourceVersion)

	if r.finalize(ctx, &pdj) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// List all associated pods
	var childPods corev1.PodList
	if err := r.List(ctx, &childPods, client.InNamespace(req.Namespace), client.MatchingFields{ctrlRefKey: req.Name}); err != nil {
		return ctrl.Result{}, err
	}

	newStatus := r.getCurrentStatus(ctx, &pdj, childPods)
	if !reflect.DeepEqual(newStatus, pdj.Status) {
		pdj.Status = newStatus
		if err := r.Status().Update(ctx, &pdj); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}

	// clean pod unnecessary
	for i, pod := range childPods.Items {
		resType, idx := extractNameIndex(pod.Name)
		if (resType == pdv1.ResourcePS && pdj.Spec.PS != nil && idx >= pdj.Spec.PS.Replicas) ||
			(resType == pdv1.ResourceWorker && pdj.Spec.Worker != nil && idx >= pdj.Spec.Worker.Replicas) {
			r.deleteResource(ctx, &pdj, &childPods.Items[i])
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	// List all associated svc
	var svcs corev1.ServiceList

	if pdj.Spec.Intranet == pdv1.Service {
		if err := r.List(ctx, &svcs, client.InNamespace(req.Namespace), client.MatchingFields{ctrlRefKey: req.Name}); err != nil {
			return ctrl.Result{}, err
		}

		// Ensure service for running pod
		for _, pod := range childPods.Items {
			svc := constructService4Pod(pod)
			if err := ctrl.SetControllerReference(&pdj, svc, r.Scheme); err != nil {
				log.Error(err, "make reference failed")
				continue
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(svc), &corev1.Service{}); err == nil {
				continue
			}
			err := r.createResource(ctx, &pdj, svc)
			return ctrl.Result{}, err
		}
	}
	if pdj.Spec.Intranet == pdv1.HostNetwork {
		if r.allocHostPortForJob(ctx, &pdj) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	cleanOne := func() {
		for i := range childPods.Items {
			r.deleteResource(ctx, &pdj, &childPods.Items[i])
			return
		}
		for i := range svcs.Items {
			r.deleteResource(ctx, &pdj, &svcs.Items[i])
			return
		}
	}

	if pdj.Status.Phase == pdv1.Failed {
		if pdj.Spec.CleanPodPolicy == pdv1.CleanAlways || pdj.Spec.CleanPodPolicy == pdv1.CleanOnFailure {
			cleanOne()
			return ctrl.Result{}, nil
		}
	}
	if pdj.Status.Phase == pdv1.Completed {
		if pdj.Spec.CleanPodPolicy == "" || pdj.Spec.CleanPodPolicy == pdv1.CleanAlways || pdj.Spec.CleanPodPolicy == pdv1.CleanOnCompletion {
			cleanOne()
			return ctrl.Result{}, nil
		}
	}

	createPod := func(resType string, idx int) bool {
		name := genPaddleResName(pdj.Name, resType, idx)
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: pdj.Namespace}, &corev1.Pod{}); err == nil {
			return false
		}
		pod := constructPod(&pdj, resType, idx)
		if err := ctrl.SetControllerReference(&pdj, pod, r.Scheme); err != nil {
			log.Error(err, "make reference failed")
			return false
		}
		if err := r.createResource(ctx, &pdj, pod); err != nil {
			log.Error(err, "create pod failed")
		}
		return true
	}

	// Ensure PS resource ready
	if pdj.Spec.PS != nil && len(pdj.Status.PS.Refs) < pdj.Spec.PS.Replicas {
		for i := 0; i < pdj.Spec.PS.Replicas; i++ {
			if createPod(pdv1.ResourcePS, i) {
				return ctrl.Result{}, nil
			}
		}
	}

	// Ensure worker resource ready
	if pdj.Spec.Worker != nil && len(pdj.Status.Worker.Refs) < pdj.Spec.Worker.Replicas {
		for i := 0; i < pdj.Spec.Worker.Replicas; i++ {
			if createPod(pdv1.ResourceWorker, i) {
				return ctrl.Result{}, nil
			}
		}
	}

	// Create configmap of global env for all pods after all pods are running
	if (pdj.Spec.PS == nil || len(pdj.Status.PS.Refs) == pdj.Spec.PS.Replicas) && (pdj.Spec.Worker == nil || len(pdj.Status.Worker.Refs) == pdj.Spec.Worker.Replicas) {
		if pdj.Spec.Intranet == pdv1.Service {
			if len(pdj.Status.PS.Refs)+len(pdj.Status.Worker.Refs) != len(svcs.Items) {
				return ctrl.Result{}, nil
			}
		}
		if err := r.Get(ctx, types.NamespacedName{Name: pdj.Name, Namespace: pdj.Namespace}, &corev1.ConfigMap{}); err == nil || !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		cm := constructConfigMap(&pdj, childPods)
		if cm == nil {
			return ctrl.Result{Requeue: true}, nil
		}
		if err := ctrl.SetControllerReference(&pdj, cm, r.Scheme); err != nil {
			log.Error(err, "make reference failed")
			return ctrl.Result{Requeue: true}, nil
		}
		err := r.createResource(ctx, &pdj, cm)
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *PaddleJobReconciler) getCurrentStatus(ctx context.Context, pdj *pdv1.PaddleJob, childPods corev1.PodList) pdv1.PaddleJobStatus {
	syncStatusByPod := func(ss *pdv1.ResourceStatus, pod *corev1.Pod) {
		if pod.CreationTimestamp.Before(&pdj.CreationTimestamp) {
			return
		}
		switch pod.Status.Phase {
		case corev1.PodPending:
			ss.Pending++
		case corev1.PodRunning:
			if isPodRealRuning(pod) {
				ss.Running++
			} else {
				ss.Starting++
			}
		case corev1.PodFailed:
			ss.Failed++
		case corev1.PodSucceeded:
			ss.Succeeded++
		}
		pref, err := ref.GetReference(r.Scheme, pod)
		if err != nil {
			return
		}
		ss.Refs = append(ss.Refs, *pref)
	}

	psStatus := pdv1.ResourceStatus{}
	workerStatus := pdv1.ResourceStatus{}
	for i, pod := range childPods.Items {
		resType := pod.Annotations[pdv1.ResourceAnnotation]
		if resType == pdv1.ResourcePS {
			syncStatusByPod(&psStatus, &childPods.Items[i])
		} else if resType == pdv1.ResourceWorker {
			syncStatusByPod(&workerStatus, &childPods.Items[i])
		}
	}

	if pdj.Spec.PS == nil {
		psStatus.Ready = "0/0"
	} else {
		psStatus.Ready = fmt.Sprintf("%d/%d", psStatus.Running, pdj.Spec.PS.Replicas)
	}
	if pdj.Spec.Worker == nil {
		workerStatus.Ready = "0/0"
	} else {
		workerStatus.Ready = fmt.Sprintf("%d/%d", workerStatus.Running, pdj.Spec.Worker.Replicas)
	}

	return pdv1.PaddleJobStatus{
		Phase:          getPaddleJobPhase(pdj),
		Mode:           getPaddleJobMode(pdj),
		PS:             psStatus,
		Worker:         workerStatus,
		StartTime:      getPaddleJobStartTime(pdj),
		CompletionTime: getPaddleJobCompleteTime(pdj),
	}
}

func (r *PaddleJobReconciler) deleteResource(ctx context.Context, pdj *pdv1.PaddleJob, obj client.Object) error {
	if obj.GetDeletionTimestamp() != nil {
		return nil
	}
	tp := obj.GetObjectKind().GroupVersionKind().Kind
	if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); (err) != nil {
		r.Recorder.Event(pdj, corev1.EventTypeWarning, "Delete", fmt.Sprintf("delete failed %s %s", tp, obj.GetName()))
		return err
	}
	r.Recorder.Event(pdj, corev1.EventTypeNormal, "Deleted", fmt.Sprintf("deleted %s %s", tp, obj.GetName()))
	return nil
}

func (r *PaddleJobReconciler) createResource(ctx context.Context, pdj *pdv1.PaddleJob, obj client.Object) error {
	tp := obj.GetObjectKind().GroupVersionKind().Kind
	if err := r.Create(ctx, obj); err != nil {
		r.Recorder.Event(pdj, corev1.EventTypeWarning, "Create", fmt.Sprintf("create failed %s %s", tp, obj.GetName()))
		return err
	}
	r.Recorder.Event(pdj, corev1.EventTypeNormal, "Created", fmt.Sprintf("created %s %s", tp, obj.GetName()))
	return nil

}

func (r *PaddleJobReconciler) allocHostPortForJob(ctx context.Context, pdj *pdv1.PaddleJob) bool {
	if pdj.Spec.Intranet != pdv1.HostNetwork {
		return false
	}
	if port, ok := pdj.ObjectMeta.Annotations[hostPort]; ok {
		// this will happen after the controler restart
		if _, okk := r.HostPortMap[port]; okk {
			return false
		} else if pdj.ObjectMeta.DeletionTimestamp.IsZero() {
			r.HostPortMap[port] = 1
			return true
		}
	} else {
		pdj.ObjectMeta.Annotations[hostPort] = fmt.Sprintf("%d", r.allocNewPort())
		r.Update(ctx, pdj)
		// ensure annotation updated
		wait.Poll(100*time.Millisecond, 2*time.Second, func() (bool, error) {
			var tmp pdv1.PaddleJob
			if errr := r.Get(ctx, types.NamespacedName{Name: pdj.Name, Namespace: pdj.Namespace}, &tmp); errr != nil {
				if _, okkk := tmp.ObjectMeta.Annotations[hostPort]; okkk {
					return true, nil
				}
			}
			return false, nil
		})
		return true
	}
	return false
}

// allocNewPort globally
func (r *PaddleJobReconciler) allocNewPort() int {
	if len(r.HostPortMap)*HOST_PORT_NUM > r.HostPortMap[HOST_PORT_END]-r.HostPortMap[HOST_PORT_START] {
		r.Log.Error(nil, "no available port")
		return 40000
	}
	if port, ok := r.HostPortMap[HOST_PORT_CUR]; ok {
		// new port prepare to allocate
		var iport = port + HOST_PORT_NUM
		if iport > r.HostPortMap[HOST_PORT_END] {
			iport = r.HostPortMap[HOST_PORT_START]
		}
		r.HostPortMap[HOST_PORT_CUR] = iport
		if _, okk := r.HostPortMap[fmt.Sprintf("%d", iport)]; okk {
			// reallocate if exists
			return r.allocNewPort()
		} else {
			r.HostPortMap[fmt.Sprintf("%d", iport)] = 1
			r.Log.V(4).Info("new port allocated", "port", iport)
			return iport
		}
	}
	r.Log.Error(nil, "something wrong with hostport allocator")
	return 40000
}

func (r *PaddleJobReconciler) finalize(ctx context.Context, pdj *pdv1.PaddleJob) bool {
	if pdj.ObjectMeta.DeletionTimestamp.IsZero() {
		if !containsString(pdj.ObjectMeta.Finalizers, finalizerName) {
			pdj.ObjectMeta.Finalizers = append(pdj.ObjectMeta.Finalizers, finalizerName)
			if err := r.Update(ctx, pdj); err != nil {
				return true
			}
		}
	} else {
		if containsString(pdj.ObjectMeta.Finalizers, finalizerName) {

			// do before delete
			if pdj.Spec.Intranet == pdv1.HostNetwork {
				if port, ok := pdj.ObjectMeta.Annotations[hostPort]; ok {
					if _, exist := r.HostPortMap[port]; exist {
						delete(r.HostPortMap, port)
						return true
					}
				}
			}

			pdj.ObjectMeta.Finalizers = removeString(pdj.ObjectMeta.Finalizers, finalizerName)
			if err := r.Update(ctx, pdj); err != nil {
				return true
			}
		}
		return false
	}
	return false
}

func indexerFunc(rawObj client.Object) []string {
	owner := metav1.GetControllerOf(rawObj)
	if owner == nil {
		return nil
	}
	// ...make sure it's a PaddleJob...
	if owner.APIVersion != apiGVStr || owner.Kind != pdv1.KIND {
		return nil
	}

	// ...and if so, return it
	return []string{owner.Name}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PaddleJobReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// index pod
	if err := mgr.GetFieldIndexer().
		IndexField(context.Background(), &corev1.Pod{}, ctrlRefKey, indexerFunc); err != nil {
		return err
	}

	// index service
	if err := mgr.GetFieldIndexer().
		IndexField(context.Background(), &corev1.Service{}, ctrlRefKey, indexerFunc); err != nil {
		return err
	}

	// index configmap
	if err := mgr.GetFieldIndexer().
		IndexField(context.Background(), &corev1.ConfigMap{}, ctrlRefKey, indexerFunc); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&pdv1.PaddleJob{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
