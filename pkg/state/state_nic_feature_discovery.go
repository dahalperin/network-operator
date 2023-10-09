/*
2023 NVIDIA CORPORATION & AFFILIATES

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

package state //nolint:dupl

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/source"

	mellanoxv1alpha1 "github.com/Mellanox/network-operator/api/v1alpha1"
	"github.com/Mellanox/network-operator/pkg/clustertype"
	"github.com/Mellanox/network-operator/pkg/config"
	"github.com/Mellanox/network-operator/pkg/consts"
	"github.com/Mellanox/network-operator/pkg/render"
	"github.com/Mellanox/network-operator/pkg/utils"
)

// NewStateNICFeatureDiscovery creates a new state for NICFeatureDiscovery
func NewStateNICFeatureDiscovery(
	k8sAPIClient client.Client, scheme *runtime.Scheme, manifestDir string) (State, error) {
	files, err := utils.GetFilesWithSuffix(manifestDir, render.ManifestFileSuffix...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get files from manifest dir")
	}

	renderer := render.NewRenderer(files)
	return &stateNICFeatureDiscovery{
		stateSkel: stateSkel{
			name:        "state-nic-feature-discovery",
			description: "nic-feature-discovery deployed in the cluster",
			client:      k8sAPIClient,
			scheme:      scheme,
			renderer:    renderer,
		}}, nil
}

type stateNICFeatureDiscovery struct {
	stateSkel
}

// nfdManifestRenderData is NIC Feature Discovery manifest rendering data
type nfdManifestRenderData struct {
	CrSpec       *mellanoxv1alpha1.NICFeatureDiscoverySpec
	NodeAffinity *v1.NodeAffinity
	Tolerations  []v1.Toleration
	RuntimeSpec  *nfdRuntimeSpec
}

type nfdRuntimeSpec struct {
	runtimeSpec
	// is true if cluster type is Openshift
	IsOpenshift bool
}

// Sync attempt to get the system to match the desired state which State represent.
// a sync operation must be relatively short and must not block the execution thread.
//
//nolint:dupl
func (s *stateNICFeatureDiscovery) Sync(
	ctx context.Context, customResource interface{}, infoCatalog InfoCatalog) (SyncState, error) {
	reqLogger := log.FromContext(ctx)
	cr := customResource.(*mellanoxv1alpha1.NicClusterPolicy)
	reqLogger.V(consts.LogLevelInfo).Info(
		"Sync Custom resource", "State:", s.name, "Name:", cr.Name, "Namespace:", cr.Namespace)

	if cr.Spec.NicFeatureDiscovery == nil {
		// Either this state was not required to run or an update occurred and we need to remove
		// the resources that where created.
		return s.handleStateObjectsDeletion(ctx)
	}

	clusterInfo := infoCatalog.GetClusterTypeProvider()
	if clusterInfo == nil {
		return SyncStateError, errors.New("unexpected state, catalog does not provide cluster type info")
	}

	// Fill ManifestRenderData and render objects
	objs, err := s.getManifestObjects(cr, clusterInfo, reqLogger)
	if err != nil {
		return SyncStateNotReady, errors.Wrap(err, "failed to create k8s objects from manifest")
	}
	if len(objs) == 0 {
		return SyncStateNotReady, nil
	}

	// Create objects if they dont exist, Update objects if they do exist
	err = s.createOrUpdateObjs(ctx, func(obj *unstructured.Unstructured) error {
		if err := controllerutil.SetControllerReference(cr, obj, s.scheme); err != nil {
			return errors.Wrap(err, "failed to set controller reference for object")
		}
		return nil
	}, objs)
	if err != nil {
		return SyncStateNotReady, errors.Wrap(err, "failed to create/update objects")
	}
	waitForStaleObjectsRemoval, err := s.handleStaleStateObjects(ctx, objs)
	if err != nil {
		return SyncStateNotReady, errors.Wrap(err, "failed to handle state stale objects")
	}
	if waitForStaleObjectsRemoval {
		return SyncStateNotReady, nil
	}
	// Check objects status
	syncState, err := s.getSyncState(ctx, objs)
	if err != nil {
		return SyncStateNotReady, errors.Wrap(err, "failed to get sync state")
	}
	return syncState, nil
}

// Get a map of source kinds that should be watched for the state keyed by the source kind name
func (s *stateNICFeatureDiscovery) GetWatchSources() map[string]*source.Kind {
	wr := make(map[string]*source.Kind)
	wr["DaemonSet"] = &source.Kind{Type: &appsv1.DaemonSet{}}
	return wr
}

func (s *stateNICFeatureDiscovery) getManifestObjects(
	cr *mellanoxv1alpha1.NicClusterPolicy,
	clusterInfo clustertype.Provider, reqLogger logr.Logger) ([]*unstructured.Unstructured, error) {
	renderData := &nfdManifestRenderData{
		CrSpec:       cr.Spec.NicFeatureDiscovery,
		NodeAffinity: cr.Spec.NodeAffinity,
		Tolerations:  cr.Spec.Tolerations,
		RuntimeSpec: &nfdRuntimeSpec{
			runtimeSpec: runtimeSpec{config.FromEnv().State.NetworkOperatorResourceNamespace},
			IsOpenshift: clusterInfo.IsOpenshift(),
		},
	}

	// render objects
	reqLogger.V(consts.LogLevelDebug).Info("Rendering objects", "data:", renderData)
	objs, err := s.renderer.RenderObjects(&render.TemplatingData{Data: renderData})

	if err != nil {
		return nil, errors.Wrap(err, "failed to render objects")
	}

	reqLogger.V(consts.LogLevelDebug).Info("Rendered", "objects:", objs)
	return objs, nil
}
