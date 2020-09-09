// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package autoscaler

import (
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/label"
	"github.com/pingcap/tidb-operator/pkg/pdapi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
)

func (am *autoScalerManager) syncPlans(tc *v1alpha1.TidbCluster, tac *v1alpha1.TidbClusterAutoScaler, plans []pdapi.Plan) error {
	groupNames := sets.String{}
	groupPlanMap := make(map[string]pdapi.Plan)
	for _, plan := range plans {
		groupName := findAutoscalingGroupNameInLabels(plan.Labels)
		groupNames.Insert(groupName)
		groupPlanMap[groupName] = plan
	}
	requirement, err := labels.NewRequirement(label.AutoScalingGroupLabelKey, selection.In, groupNames.List())
	if err != nil {
		return err
	}
	selector := labels.NewSelector().Add(*requirement)

	tcList, err := am.tcLister.List(selector)
	if err != nil {
		return err
	}

	existedGroups := sets.String{}
	groupTcMap := make(map[string]*v1alpha1.TidbCluster)
	for _, tc := range tcList {
		groupName := tc.Labels[label.AutoScalingGroupLabelKey]
		existedGroups.Insert(groupName)
		groupTcMap[groupName] = tc
	}

	toDelete := existedGroups.Difference(groupNames)
	err = am.deleteAutoscalingClusters(tc, toDelete.UnsortedList(), groupTcMap)
	if err != nil {
		return err
	}

	toUpdate := groupNames.Intersection(existedGroups)
	err = am.updateAutoscalingClusters(toUpdate.UnsortedList(), groupTcMap, groupPlanMap)
	if err != nil {
		return err
	}

	toCreate := groupNames.Difference(existedGroups)
	err = am.createAutoscalingClusters(tc, tac, toCreate.UnsortedList(), groupPlanMap)
	if err != nil {
		return err
	}

	return nil
}

func (am *autoScalerManager) deleteAutoscalingClusters(tc *v1alpha1.TidbCluster, groupsToDelete []string, groupTcMap map[string]*v1alpha1.TidbCluster) error {
	for _, group := range groupsToDelete {
		deleteTc := groupTcMap[group]
		err := am.cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Delete(deleteTc.Name, nil)
		if err != nil {
			return err
		}

		// Remove the cluster in the monitor
		if monitorRef := deleteTc.Status.Monitor; monitorRef != nil {
			monitor, err := am.tmLister.TidbMonitors(monitorRef.Namespace).Get(monitorRef.Name)
			if err != nil {
				return err
			}
			updated := monitor.DeepCopy()
			clusters := make([]v1alpha1.TidbClusterRef, 0, len(updated.Spec.Clusters)-1)
			for _, cluster := range updated.Spec.Clusters {
				if cluster.Name != deleteTc.Name {
					clusters = append(clusters, cluster)
				}
			}
			updated.Spec.Clusters = clusters
			_, err = am.cli.PingcapV1alpha1().TidbMonitors(monitor.Namespace).Update(updated)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (am *autoScalerManager) updateAutoscalingClusters(groups []string, groupTcMap map[string]*v1alpha1.TidbCluster, groupPlanMap map[string]pdapi.Plan) error {
	for _, group := range groups {
		actual, oldTc, expected := groupTcMap[group].DeepCopy(), groupTcMap[group], groupPlanMap[group]
		component := expected.Component

		switch component {
		case "tikv":
			actual.Spec.TiKV.Replicas = int32(expected.Count)
		case "tidb":
			actual.Spec.TiDB.Replicas = int32(expected.Count)
		}

		_, err := am.tcControl.UpdateTidbCluster(actual, &actual.Status, &oldTc.Status)
		if err != nil {
			return err
		}
	}
	return nil
}

func (am *autoScalerManager) createAutoscalingClusters(tc *v1alpha1.TidbCluster, tac *v1alpha1.TidbClusterAutoScaler, groupsToCreate []string, groupPlanMap map[string]pdapi.Plan) error {
	for _, group := range groupsToCreate {
		plan := groupPlanMap[group]
		component := plan.Component
		labels := make(map[string]string)
		for _, label := range plan.Labels {
			labels[label.Key] = label.Value
		}

		var resource v1alpha1.AutoResource
		for _, res := range tac.Spec.Resources {
			if res.ResourceType == plan.ResourceType {
				resource = res
				break
			}
		}
		resList := corev1.ResourceList{
			corev1.ResourceCPU:     resource.CPU,
			corev1.ResourceStorage: resource.Storage,
			corev1.ResourceMemory:  resource.Memory,
		}
		tc := &v1alpha1.TidbCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      group,
				Namespace: tc.Namespace,
			},
			Spec: v1alpha1.TidbClusterSpec{
				Cluster: &v1alpha1.TidbClusterRef{
					Name:      tc.Name,
					Namespace: tc.Namespace,
				},
			},
		}

		switch component {
		case "tikv":
			tc.Spec.TiKV = &v1alpha1.TiKVSpec{
				Replicas: int32(plan.Count),
				ResourceRequirements: corev1.ResourceRequirements{
					Limits:   resList,
					Requests: resList,
				},
				Config: &v1alpha1.TiKVConfig{
					Server: &v1alpha1.TiKVServerConfig{
						Labels: labels,
					},
				},
			}
		case "tidb":
			tc.Spec.TiDB = &v1alpha1.TiDBSpec{
				Replicas: int32(plan.Count),
				ResourceRequirements: corev1.ResourceRequirements{
					Limits:   resList,
					Requests: resList,
				},
				Config: &v1alpha1.TiDBConfig{
					Labels: labels,
				},
			}
		}

		created, err := am.cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Create(tc)
		if err != nil {
			klog.Errorf("cannot create new TidbCluster %v\n", err)
			return err
		}

		if monitorRef := tc.Status.Monitor; monitorRef != nil {
			monitor, err := am.tmLister.TidbMonitors(monitorRef.Namespace).Get(monitorRef.Name)
			if err != nil {
				return err
			}
			updated := monitor.DeepCopy()
			updated.Spec.Clusters = append(updated.Spec.Clusters, v1alpha1.TidbClusterRef{Name: created.Name, Namespace: created.Namespace})
			am.cli.PingcapV1alpha1().TidbMonitors(updated.Namespace).Update(updated)
		}
	}
	return nil
}
