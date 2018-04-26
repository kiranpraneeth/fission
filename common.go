/*
Copyright 2016 The Fission Authors.

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

package fission

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/gorilla/handlers"
	"github.com/imdario/mergo"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	rbac "k8s.io/client-go/pkg/apis/rbac/v1beta1"
)

func UrlForFunction(name, namespace string) string {
	prefix := "/fission-function"
	if namespace != metav1.NamespaceDefault {
		prefix += "/" + namespace
	}
	return fmt.Sprintf("%v/%v", prefix, name)
}

func SetupStackTraceHandler() {
	// register signal handler for dumping stack trace.
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Println("Received SIGTERM : Dumping stack trace")
		debug.PrintStack()
		os.Exit(1)
	}()
}

// IsNetworkError returns true if an error is a network error, and false otherwise.
func IsNetworkError(err error) bool {
	_, ok := err.(net.Error)
	return ok
}

// GetFunctionIstioServiceName return service name of function for istio feature
func GetFunctionIstioServiceName(fnName, fnNamespace string) string {
	return fmt.Sprintf("istio-%v-%v", fnName, fnNamespace)
}

func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI := r.RequestURI
		if !strings.Contains(requestURI, "healthz") {
			// Call the next handler, which can be another middleware in the chain, or the final handler.
			handlers.LoggingHandler(os.Stdout, next).ServeHTTP(w, r)
		}
	})
}

// MergeContainerSpecs merges container specs using a predefined order.
//
// The order of the arguments indicates which spec has precedence (lower index takes precedence over higher indexes).
// Slices and maps are merged; other fields are set only if they are a zero value.
func MergeContainerSpecs(specs ...*apiv1.Container) apiv1.Container {
	result := &apiv1.Container{}
	for _, spec := range specs {
		if spec == nil {
			continue
		}

		err := mergo.Merge(result, spec)
		if err != nil {
			panic(err)
		}
	}
	return *result
}

// IsNetworkDialError returns true if its a network dial error
func IsNetworkDialError(err error) bool {
	netErr, ok := err.(net.Error)
	if !ok {
		return false
	}
	netOpErr, ok := netErr.(*net.OpError)
	if !ok {
		return false
	}
	if netOpErr.Op == "dial" {
		return true
	}
	return false
}

// IsReadyPod checks that all containers in a pod are ready and returns true if so
func IsReadyPod(pod *apiv1.Pod) bool {
	// since its a utility function, just ensuring there is no nil pointer exception
	if pod == nil {
		return false
	}

	for _, cStatus := range pod.Status.ContainerStatuses {
		if !cStatus.Ready {
			return false
		}
	}

	return true
}

func MakeSAObj(sa, ns string) *apiv1.ServiceAccount {
	return &apiv1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      sa,
		},
	}
}

func SetupSA(k8sClient *kubernetes.Clientset, sa, ns string) (*apiv1.ServiceAccount, error) {
	saObj, err := k8sClient.CoreV1Client.ServiceAccounts(ns).Get(sa, metav1.GetOptions{})
	if err == nil {
		return saObj, nil
	}

	if k8serrors.IsNotFound(err) {
		saObj = MakeSAObj(sa, ns)
		saObj, err = k8sClient.CoreV1Client.ServiceAccounts(ns).Create(saObj)
	}

	return saObj, err
}

func makeRoleBindingObj(roleBinding, roleBindingNs, role, roleKind, sa, saNamespace string) *rbac.RoleBinding {
	return &rbac.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleBinding,
			Namespace: roleBindingNs,
		},
		Subjects: []rbac.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      sa,
				Namespace: saNamespace,
			},
		},
		RoleRef: rbac.RoleRef{
			Kind: roleKind,
			Name: role,
		},
	}
}

func isSAInRoleBinding(rbObj *rbac.RoleBinding, sa, ns string) bool {
	for _, subject := range rbObj.Subjects {
		if subject.Name == sa && subject.Namespace == ns {
			return true
		}
	}

	return false
}

// TODO : Convert all prints to debugs after testing.
type PatchSpec struct {
	Op    string       `json:"op"`
	Path  string       `json:"path"`
	Value rbac.Subject `json:"value"`
}

func AddSaToRoleBinding(k8sClient *kubernetes.Clientset, roleBinding, roleBindingNs, sa, saNamespace string) error {
	patch := PatchSpec{}
	patch.Op = "add"
	patch.Path = "/subjects/-"
	patch.Value = rbac.Subject{
		Kind:      "ServiceAccount",
		Name:      sa,
		Namespace: saNamespace,
	}

	patchJson, err := json.Marshal([]PatchSpec{patch})
	if err != nil {
		log.Printf("Error marshalling patch into json")
		return err
	}

	log.Printf("Final patchJson : %s", string(patchJson))

	_, err = k8sClient.RbacV1beta1().RoleBindings(roleBindingNs).Patch(roleBinding, types.JSONPatchType, patchJson)
	return err
}

// TODO : Remove these prints once testing is done.
func UpdateRoleBindingWithRetries(k8sClient *kubernetes.Clientset, rbObj *rbac.RoleBinding) error {
	for {
		log.Printf("calling update on rolebinding for object : %v", rbObj)
		_, err := k8sClient.RbacV1beta1().RoleBindings(rbObj.Namespace).Update(rbObj)
		switch {
		case err == nil:
			return nil
		case k8serrors.IsConflict(err):
			log.Printf("Conflict in update, retrying")
			continue
		default:
			log.Printf("Errored out : %v", err)
			return err
		}
	}
}

func RemoveSAFromRoleBinding(k8sClient *kubernetes.Clientset, roleBinding, roleBindingNs, sa, ns string) error {
	rbObj, err := k8sClient.RbacV1beta1().RoleBindings(roleBindingNs).Get(
		roleBinding, metav1.GetOptions{})
	if err != nil {
		// silently ignoring the error. there's no need for us to remove sa anymore.
		log.Printf("rolebinding %s.%s not found", roleBinding, roleBindingNs)
		return nil
	}

	subjects := rbObj.Subjects
	newSubjects := make([]rbac.Subject, 0)

	// TODO : optimize it.
	for _, item := range subjects {
		if item.Name == sa && item.Namespace == ns {
			continue
		}

		newSubjects = append(newSubjects, rbac.Subject{
			Kind:      "ServiceAccount",
			Name:      item.Name,
			Namespace: item.Namespace,
		})
	}
	rbObj.Subjects = newSubjects

	// cant use patch for deletes, the results become undeterministic. so, retrying for IsConflict errors
	return UpdateRoleBindingWithRetries(k8sClient, rbObj)
}

func SetupRoleBinding(k8sClient *kubernetes.Clientset, roleBinding, roleBindingNs, role, roleKind, sa, saNamespace string) error {
	// get the role binding object
	rbObj, err := k8sClient.RbacV1beta1().RoleBindings(roleBindingNs).Get(
		roleBinding, metav1.GetOptions{})

	if err == nil {
		if !isSAInRoleBinding(rbObj, sa, saNamespace) {
			return AddSaToRoleBinding(k8sClient, roleBinding, roleBindingNs, sa, saNamespace)
		}
		return nil
	}

	// if role binding is missing, create it. also add this sa to the binding.
	if k8serrors.IsNotFound(err) {
		log.Printf("Rolebinding %s does NOT exist in ns %s. Creating it", roleBinding, roleBindingNs)
		rbObj = makeRoleBindingObj(roleBinding, roleBindingNs, role, roleKind, sa, saNamespace)
		rbObj, err = k8sClient.RbacV1beta1().RoleBindings(roleBindingNs).Create(rbObj)
		if k8serrors.IsAlreadyExists(err) {
			// patch
			log.Printf("Conflict or already exists")
			err = AddSaToRoleBinding(k8sClient, roleBinding, roleBindingNs, sa, saNamespace)
		}
	}

	return err
}

func DeleteRoleBinding(k8sClient *kubernetes.Clientset, roleBinding, roleBindingNs string) error {
	// if deleteRoleBinding is invoked by 2 fission services at the same time for the same rolebinding,
	// the first call will succeed while the 2nd will fail with isNotFound. but we dont want to error out then.
	err := k8sClient.RbacV1beta1().RoleBindings(roleBindingNs).Delete(roleBinding, &metav1.DeleteOptions{})
	if err == nil || k8serrors.IsNotFound(err) {
		return nil
	}
	return err
}
