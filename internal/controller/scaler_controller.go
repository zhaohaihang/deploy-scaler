/*
Copyright 2024.

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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	// v1 "k8s.io/client-go/applyconfigurations/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	apiv1alpha1 "github.com/zhaohaihang/deploy-scaler/api/v1alpha1"
)

const finalizer = "scalers.api.scaler.com/finalizer"

var logger = log.Log.WithName("scaler-controller")

var originalDeploymentInfo = make(map[string]apiv1alpha1.DeploymentInfo)
var annotations = make(map[string]string)

// ScalerReconciler reconciles a Scaler object
type ScalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=api.scaler.com,resources=scalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=api.scaler.com,resources=scalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=api.scaler.com,resources=scalers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Scaler object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.2/pkg/reconcile
func (r *ScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)
	log.Info("Request called")

	// 创建一个scaler实例
	scaler := &apiv1alpha1.Scaler{}
	if err := r.Get(ctx, req.NamespacedName, scaler);err != nil {
		// 如果没有scaler实例，就使用IgnoreNotFound 忽略错误，使进程不中断
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if scaler.ObjectMeta.DeletionTimestamp.IsZero(){
		if !controllerutil.ContainsFinalizer(scaler,finalizer){
			controllerutil.AddFinalizer(scaler,finalizer)
			log.Info("add finalizer")
			if err := r.Update(ctx,scaler);err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}

		if scaler.Status.Status == "" {
			scaler.Status.Status = apiv1alpha1.PENDING
			if err := r.Status().Update(ctx, scaler); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
	
			if err := addAnnotations(ctx, scaler, r); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
	
		startTime := scaler.Spec.Start
		endTime := scaler.Spec.End
		replicas := scaler.Spec.Replicas
	
		currenHour := time.Now().Local().Hour()
		log.Info(fmt.Sprintf("currentTime: %d", currenHour))
	
		if currenHour >= startTime && currenHour < endTime {
			if scaler.Status.Status != apiv1alpha1.SCALED {
				log.Info("starting to call scaleDeployment func.")
				if err := scaleDeployment(scaler, r, ctx, replicas); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else {
			if scaler.Status.Status == apiv1alpha1.SCALED {
				restoreDeployment(ctx, scaler, r)
			}
		}
	}else{
		log.Info("start deletion flow")
		if scaler.Status.Status == apiv1alpha1.SCALED {
			if err := restoreDeployment(ctx,scaler,r); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("remove finalinzer")
			controllerutil.RemoveFinalizer(scaler,finalizer)
			if err := r.Update(ctx,scaler);err != nil {
				return ctrl.Result{}, err
			}
		}
		log.Info("remove scaler")
	}
	
	return ctrl.Result{RequeueAfter: (10 * time.Second)}, nil
}

func restoreDeployment(ctx context.Context, scaler *apiv1alpha1.Scaler, r *ScalerReconciler) error {

	logger.Info("starting to return to the original state")
	for originalDeploymentName, originalDeploymentInfo := range originalDeploymentInfo {
		deployment := &v1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      originalDeploymentName,
			Namespace: originalDeploymentInfo.Namespace,
		}, deployment); err != nil {
			return err
		}

		if deployment.Spec.Replicas != &originalDeploymentInfo.Replicas {
			deployment.Spec.Replicas = &originalDeploymentInfo.Replicas
			if err := r.Update(ctx, deployment); err != nil {
				return err
			}
		}
	}

	scaler.Status.Status = apiv1alpha1.RESTORED
	r.Status().Update(ctx, scaler)

	return nil
}

func scaleDeployment(scaler *apiv1alpha1.Scaler, r *ScalerReconciler, ctx context.Context, replicas int32) error {
	// 从scaler 遍历deployment
	for _, deploy := range scaler.Spec.Deployments {
		deployment := &v1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		}, deployment)
		if err != nil {

			return err
		}
		// 检查集群中当前deployment数量是否符合scaler预期
		if deployment.Spec.Replicas != &replicas {
			deployment.Spec.Replicas = &replicas
			err := r.Update(ctx, deployment)
			if err != nil {
				scaler.Status.Status = apiv1alpha1.FAILED
				r.Status().Update(ctx, scaler)
				return err
			}
			scaler.Status.Status = apiv1alpha1.SCALED
			r.Status().Update(ctx, scaler)
		}
	}
	return nil
}

func addAnnotations(ctx context.Context, scaler *apiv1alpha1.Scaler, r *ScalerReconciler) error {

	// 记录deployment的原始副本数和namespace名称
	for _, deploy := range scaler.Spec.Deployments {
		deployment := &v1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		}, deployment); err != nil {
			return err
		}

		if deployment.Spec.Replicas != &scaler.Spec.Replicas {
			logger.Info("add original state to originalDeploymentInfo map")
			originalDeploymentInfo[deployment.Name] = apiv1alpha1.DeploymentInfo{
				Namespace: deployment.Namespace,
				Replicas:  *deployment.Spec.Replicas,
			}
		}
	}

	for deploymentName, info := range originalDeploymentInfo {
		infoJson, err := json.Marshal(info)
		if err != nil {
			return err
		}
		annotations[deploymentName] = string(infoJson)
	}

	scaler.ObjectMeta.Annotations = annotations
	err := r.Update(ctx, scaler)
	if err != nil {
		return err
	}

	return nil

}

// SetupWithManager sets up the controller with the Manager.
func (r *ScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1alpha1.Scaler{}).
		Complete(r)
}
