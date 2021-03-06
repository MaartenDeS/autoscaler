/*
Copyright 2019 The Kubernetes Authors.

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

package controllerfetcher

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	kube_client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

type wellKnownController string

const (
	daemonSet             wellKnownController = "DaemonSet"
	deployment            wellKnownController = "Deployment"
	replicaSet            wellKnownController = "ReplicaSet"
	statefulSet           wellKnownController = "StatefulSet"
	replicationController wellKnownController = "ReplicationController"
	job                   wellKnownController = "Job"
)

var wellKnownControllers = []wellKnownController{daemonSet, deployment, replicaSet, statefulSet, replicationController, job}

const (
	discoveryResetPeriod time.Duration = 5 * time.Minute
)

// ControllerKey identifies a controller.
type ControllerKey struct {
	Namespace string
	Kind      string
	Name      string
}

// ControllerKeyWithAPIVersion identifies a controller and API it's defined in.
type ControllerKeyWithAPIVersion struct {
	ControllerKey
	ApiVersion string
}

// ControllerFetcher is responsible for finding the top level controller
type ControllerFetcher interface {
	// FindTopLevel returns top level controller. Error is returned if top level controller cannot be found.
	FindTopLevel(controller *ControllerKeyWithAPIVersion) (*ControllerKeyWithAPIVersion, error)
}

type controllerFetcher struct {
	scaleNamespacer scale.ScalesGetter
	mapper          apimeta.RESTMapper
	informersMap    map[wellKnownController]cache.SharedIndexInformer
}

// NewControllerFetcher returns a new instance of controllerFetcher
func NewControllerFetcher(config *rest.Config, kubeClient kube_client.Interface, factory informers.SharedInformerFactory) ControllerFetcher {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		klog.Fatalf("Could not create discoveryClient: %v", err)
	}
	resolver := scale.NewDiscoveryScaleKindResolver(discoveryClient)
	restClient := kubeClient.CoreV1().RESTClient()
	cachedDiscoveryClient := cacheddiscovery.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)
	go wait.Until(func() {
		mapper.Reset()
	}, discoveryResetPeriod, make(chan struct{}))

	informersMap := map[wellKnownController]cache.SharedIndexInformer{
		daemonSet:             factory.Apps().V1().DaemonSets().Informer(),
		deployment:            factory.Apps().V1().Deployments().Informer(),
		replicaSet:            factory.Apps().V1().ReplicaSets().Informer(),
		statefulSet:           factory.Apps().V1().StatefulSets().Informer(),
		replicationController: factory.Core().V1().ReplicationControllers().Informer(),
		job:                   factory.Batch().V1().Jobs().Informer(),
	}

	for kind, informer := range informersMap {
		stopCh := make(chan struct{})
		go informer.Run(stopCh)
		synced := cache.WaitForCacheSync(stopCh, informer.HasSynced)
		if !synced {
			klog.Warningf("Could not sync cache for %s: %v", kind, err)
		} else {
			klog.Infof("Initial sync of %s completed", kind)
		}
	}

	scaleNamespacer := scale.New(restClient, mapper, dynamic.LegacyAPIPathResolverFunc, resolver)
	return &controllerFetcher{
		scaleNamespacer: scaleNamespacer,
		mapper:          mapper,
		informersMap:    informersMap,
	}
}

func getOwnerController(owners []metav1.OwnerReference, namespace string) *ControllerKeyWithAPIVersion {
	for _, owner := range owners {
		if owner.Controller != nil && *owner.Controller == true {
			return &ControllerKeyWithAPIVersion{
				ControllerKey: ControllerKey{
					Namespace: namespace,
					Kind:      owner.Kind,
					Name:      owner.Name,
				},
				ApiVersion: owner.APIVersion,
			}
		}
	}
	return nil
}

func getParentOfWellKnownController(informer cache.SharedIndexInformer, controllerKey ControllerKeyWithAPIVersion) (*ControllerKeyWithAPIVersion, error) {
	namespace := controllerKey.Namespace
	name := controllerKey.Name
	kind := controllerKey.Kind

	obj, exists, err := informer.GetStore().GetByKey(namespace + "/" + name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%s %s/%s does not exist", kind, namespace, name)
	}
	switch obj.(type) {
	case (*appsv1.DaemonSet):
		apiObj, ok := obj.(*appsv1.DaemonSet)
		if !ok {
			return nil, fmt.Errorf("Failed to parse %s %s/%s", kind, namespace, name)
		}
		return getOwnerController(apiObj.OwnerReferences, namespace), nil
	case (*appsv1.Deployment):
		apiObj, ok := obj.(*appsv1.Deployment)
		if !ok {
			return nil, fmt.Errorf("Failed to parse %s %s/%s", kind, namespace, name)
		}
		return getOwnerController(apiObj.OwnerReferences, namespace), nil
	case (*appsv1.StatefulSet):
		apiObj, ok := obj.(*appsv1.StatefulSet)
		if !ok {
			return nil, fmt.Errorf("Failed to parse %s %s/%s", kind, namespace, name)
		}
		return getOwnerController(apiObj.OwnerReferences, namespace), nil
	case (*appsv1.ReplicaSet):
		apiObj, ok := obj.(*appsv1.ReplicaSet)
		if !ok {
			return nil, fmt.Errorf("Failed to parse %s %s/%s", kind, namespace, name)
		}
		return getOwnerController(apiObj.OwnerReferences, namespace), nil
	case (*batchv1.Job):
		apiObj, ok := obj.(*batchv1.Job)
		if !ok {
			return nil, fmt.Errorf("Failed to parse %s %s/%s", kind, namespace, name)
		}
		return getOwnerController(apiObj.OwnerReferences, namespace), nil
	case (*corev1.ReplicationController):
		apiObj, ok := obj.(*corev1.ReplicationController)
		if !ok {
			return nil, fmt.Errorf("Failed to parse %s %s/%s", kind, namespace, name)
		}
		return getOwnerController(apiObj.OwnerReferences, namespace), nil
	}

	return nil, fmt.Errorf("Don't know how to read owner controller")
}

func (f *controllerFetcher) getParentOfController(controllerKey ControllerKeyWithAPIVersion) (*ControllerKeyWithAPIVersion, error) {
	kind := wellKnownController(controllerKey.Kind)
	informer, exists := f.informersMap[kind]
	if exists {
		return getParentOfWellKnownController(informer, controllerKey)
	}

	// TODO: cache response
	groupVersion, err := schema.ParseGroupVersion(controllerKey.ApiVersion)
	if err != nil {
		return nil, err
	}
	groupKind := schema.GroupKind{
		Group: groupVersion.Group,
		Kind:  controllerKey.Kind,
	}

	owner, err := f.getOwnerForScaleResource(groupKind, controllerKey.Namespace, controllerKey.Name)
	if err != nil {
		return nil, fmt.Errorf("Unhandled targetRef %s / %s / %s, last error %v",
			controllerKey.ApiVersion, controllerKey.Kind, controllerKey.Name, err)
	}

	return owner, nil
}

func (f *controllerFetcher) getOwnerForScaleResource(groupKind schema.GroupKind, namespace, name string) (*ControllerKeyWithAPIVersion, error) {
	mappings, err := f.mapper.RESTMappings(groupKind)
	if err != nil {
		return nil, err
	}

	var lastError error
	for _, mapping := range mappings {
		groupResource := mapping.Resource.GroupResource()
		scale, err := f.scaleNamespacer.Scales(namespace).Get(groupResource, name)
		if err == nil {
			return getOwnerController(scale.OwnerReferences, namespace), nil
		}
		lastError = err
	}

	// nothing found, apparently the resource doesn't support scale (or we lack RBAC)
	return nil, lastError
}

func (f *controllerFetcher) FindTopLevel(key *ControllerKeyWithAPIVersion) (*ControllerKeyWithAPIVersion, error) {
	if key == nil {
		return nil, nil
	}
	visited := make(map[ControllerKeyWithAPIVersion]bool)
	visited[*key] = true
	for {
		owner, err := f.getParentOfController(*key)
		if err != nil {
			return nil, err
		}
		if owner == nil {
			return key, nil
		}
		_, alreadyVisited := visited[*owner]
		if alreadyVisited {
			return nil, fmt.Errorf("Cycle detected in ownership chain")
		}
		visited[*key] = true
		key = owner
	}
}

type identityControllerFetcher struct {
}

func (f *identityControllerFetcher) FindTopLevel(controller *ControllerKeyWithAPIVersion) (*ControllerKeyWithAPIVersion, error) {
	return controller, nil
}

type constControllerFetcher struct {
	ControllerKeyWithAPIVersion *ControllerKeyWithAPIVersion
}

func (f *constControllerFetcher) FindTopLevel(controller *ControllerKeyWithAPIVersion) (*ControllerKeyWithAPIVersion, error) {
	return f.ControllerKeyWithAPIVersion, nil
}

type mockControllerFetcher struct {
	expected *ControllerKeyWithAPIVersion
	result   *ControllerKeyWithAPIVersion
}

func (f *mockControllerFetcher) FindTopLevel(controller *ControllerKeyWithAPIVersion) (*ControllerKeyWithAPIVersion, error) {
	if controller == nil && f.expected == nil {
		return f.result, nil
	}
	if controller == nil || *controller != *f.expected {
		return nil, fmt.Errorf("Unexpected argument: %v", controller)
	}

	return f.result, nil
}
