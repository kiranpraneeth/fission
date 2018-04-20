/*
Copyright 2018 The Fission Authors.

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

package poolmgr

import (
	"time"

	log "github.com/sirupsen/logrus"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/rest"
	k8sCache "k8s.io/client-go/tools/cache"

	"github.com/fission/fission"
	"github.com/fission/fission/crd"
)

func getIstioServiceLabels(fnName string) map[string]string {
	return map[string]string{
		"functionName": fnName,
	}
}

func makeFuncController(crdClient *rest.RESTClient,
	kubernetesClient *kubernetes.Clientset, fissionfnNamespace string, istioEnabled bool) k8sCache.Controller {

	resyncPeriod := 30 * time.Second
	lw := k8sCache.NewListWatchFromClient(crdClient, "functions", metav1.NamespaceAll, fields.Everything())
	_, controller := k8sCache.NewInformer(lw, &crd.Function{}, resyncPeriod,
		k8sCache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				log.Printf("List watch for function reported a new function addition")
				fn := obj.(*crd.Function)

				// Since istio only allows accessing pod through k8s service,
				// for the functions with executor type "poolmgr" we need to
				// create a service for sending requests to pod in pool.
				// Functions with executor type "Newdeploy" is specialized at
				// pod starts. In this case, just ignore such functions.
				fnExecutorType := fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType
				if fnExecutorType != fission.ExecutorTypePoolmgr {
					return
				}

				// create or update role-binding
				// SetupRoleBinding(k8sClient *kubernetes.Clientset, sa, ns, roleBinding, clusterRole string, roleBindingNs string)
				envNs := fissionfnNamespace
				if fn.Spec.Environment.Namespace != metav1.NamespaceDefault {
					envNs = fn.Spec.Environment.Namespace
				}

				err := fission.SetupRoleBinding(kubernetesClient, fission.GetSecretConfigMapRoleBinding, fn.Metadata.Namespace, fission.SecretConfigMapGetterCR, fission.ClusterRole, fission.FissionFetcherSA, envNs)
				if err != nil {
					log.Printf("Error : %v creating %s clusterRole", err, fission.GetSecretConfigMapRoleBinding)
				}

				log.Printf("Successfully set up rolebinding for fetcher SA: %s.%s, in func's ns : %s, for func : %s", fission.FissionFetcherSA, envNs, fn.Metadata.Namespace, fn.Metadata.Name)

				if istioEnabled {
					// create a same name service for function
					// since istio only allows the traffic to service
					sel := map[string]string{
						"functionName": fn.Metadata.Name,
						"functionUid":  string(fn.Metadata.UID),
					}

					svcName := fission.GetFunctionIstioServiceName(fn.Metadata.Name, fn.Metadata.Namespace)

					// service for accepting user traffic
					svc := apiv1.Service{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: envNs,
							Name:      svcName,
							Labels:    getIstioServiceLabels(fn.Metadata.Name),
						},
						Spec: apiv1.ServiceSpec{
							Type: apiv1.ServiceTypeClusterIP,
							Ports: []apiv1.ServicePort{
								// Service port name should begin with a recognized prefix, or the traffic will be
								// treated as TCP traffic. (https://istio.io/docs/setup/kubernetes/sidecar-injection.html)
								// Originally the ports' name are similar to "http-fetch" and "http-specialize".
								// But for istio 0.5.1, istio-proxy return unexpected 431 error with such naming.
								// https://github.com/istio/istio/issues/928
								// Workaround: remove prefix
								// TODO: prepend prefix once the bug fixed
								{
									Name:       "fetch",
									Protocol:   apiv1.ProtocolTCP,
									Port:       8000,
									TargetPort: intstr.FromInt(8000),
								},
								{
									Name:       "specialize",
									Protocol:   apiv1.ProtocolTCP,
									Port:       8888,
									TargetPort: intstr.FromInt(8888),
								},
							},
							Selector: sel,
						},
					}

					// create function istio service if it does not exist
					_, err = kubernetesClient.CoreV1().Services(envNs).Create(&svc)
					if err != nil && !kerrors.IsAlreadyExists(err) {
						log.Printf("Error creating function istio service: %v", err)
					}
				}
			},

			DeleteFunc: func(obj interface{}) {
				fn := obj.(*crd.Function)

				// TODO : Remove rolebinding for this function only if there's no other function in same ns
				// using the same env

				envNs := fissionfnNamespace
				if fn.Spec.Environment.Namespace != metav1.NamespaceDefault {
					envNs = fn.Spec.Environment.Namespace
				}

				if istioEnabled {
					svcName := fission.GetFunctionIstioServiceName(fn.Metadata.Name, fn.Metadata.Namespace)
					// delete function istio service
					err := kubernetesClient.CoreV1().Services(envNs).Delete(svcName, nil)
					if err != nil && !kerrors.IsNotFound(err) {
						log.Printf("Error deleting function istio service: %v", err)
					}
				}
			},

			UpdateFunc: func(oldObj, newObj interface{}) {
				oldFunc := oldObj.(*crd.Function)
				newFunc := newObj.(*crd.Function)

				// if a func's env reference gets updated and the newly referenced env is in a different ns,
				// we need to create a rolebinding in func's ns so that the fetcher-sa in env ns has access
				// to fetch secrets and config maps from the func's ns.
				if oldFunc.Spec.Environment.Namespace != newFunc.Spec.Environment.Namespace {
					log.Printf("Setting up rolebinding for fetcher SA in func's env ns : %s, in func's ns : %s, for func : %s", newFunc.Spec.Environment.Namespace, newFunc.Metadata.Namespace, newFunc.Metadata.Name)
					envNs := fissionfnNamespace
					if newFunc.Spec.Environment.Namespace != metav1.NamespaceDefault {
						envNs = newFunc.Spec.Environment.Namespace
					}

					err := fission.SetupRoleBinding(kubernetesClient, fission.GetSecretConfigMapRoleBinding,
						newFunc.Metadata.Namespace, fission.SecretConfigMapGetterCR, fission.ClusterRole,
						fission.FissionFetcherSA, envNs)
					if err != nil {
						log.Printf("Error : %v creating GetSecretConfigMapRoleBinding", err)
					}

					log.Printf("Successfully set up rolebinding for fetcher SA in func's env ns : %s, in func's ns : %s, for func : %s", newFunc.Spec.Environment.Namespace, newFunc.Metadata.Namespace, newFunc.Metadata.Name)
				}
			},
		})

	return controller
}
