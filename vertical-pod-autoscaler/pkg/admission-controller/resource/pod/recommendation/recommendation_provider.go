/*
Copyright 2018 The Kubernetes Authors.

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

package recommendation

import (
	"flag"
	"fmt"
	"k8s.io/apimachinery/pkg/api/resource"
	"math"
	"strings"

	core "k8s.io/api/core/v1"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/limitrange"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
	"k8s.io/klog/v2"
)

const (
	metadataCopyPrefix            = "vpa.datadoghq.com/"
	annotationObjectType          = metadataCopyPrefix + "type"
	annotationPreferredUpdateMode = metadataCopyPrefix + "updateMode"
	annotationQosEnable           = metadataCopyPrefix + "qosEnable"
)

var (
	percentageFlag      = flag.Int("percentage", 0, "Percent (int, 0-100) of recommendation to use over original request.")
	percentErrorPrinted = false
)

// Provider gets current recommendation, annotations and vpaName for the given pod.
type Provider interface {
	GetContainersResourcesForPod(pod *core.Pod, vpa *vpa_types.VerticalPodAutoscaler) ([]vpa_api_util.ContainerResources, vpa_api_util.ContainerToAnnotationsMap, error)
}

type recommendationProvider struct {
	limitsRangeCalculator   limitrange.LimitRangeCalculator
	recommendationProcessor vpa_api_util.RecommendationProcessor
	enableQoS               bool
}

// NewProvider constructs the recommendation provider that can be used to determine recommendations for pods.
func NewProvider(calculator limitrange.LimitRangeCalculator,
	recommendationProcessor vpa_api_util.RecommendationProcessor, enableQoS bool) Provider {
	return &recommendationProvider{
		limitsRangeCalculator:   calculator,
		recommendationProcessor: recommendationProcessor,
		enableQoS:               enableQoS,
	}
}

// percentageToTarget returns an interpolated value between requests and target, where a proportion of 0 is requests, and 1.0 is target.
func percentageToTarget(requests core.ResourceList, target core.ResourceList, proportion float64, inQoS bool) core.ResourceList {
	reqMem, okReq := requests.Memory().AsInt64()
	targetMem, okTarget := target.Memory().AsInt64()
	if !okReq || !okTarget {
		return requests
	}
	cpuDiff := target.Cpu().MilliValue() - requests.Cpu().MilliValue()
	memDiff := targetMem - reqMem

	// For QoS'd workloads, where the target core count is an integer, round the proportion to an integer too.
	cpuQuantity := requests.Cpu().MilliValue() + int64(float64(cpuDiff)*proportion)
	if inQoS && target.Cpu().MilliValue()%1000 == 0 {
		// This is generalized for whether the target or request are larger.
		minBound := int64(math.Min(float64(target.Cpu().MilliValue()), float64(requests.Cpu().MilliValue())))
		maxBound := int64(math.Max(float64(target.Cpu().MilliValue()), float64(requests.Cpu().MilliValue())))
		remainder := cpuQuantity % 1000
		upDelta := 1000 - remainder
		if remainder != 0 {
			if remainder < 500 {
				// Try to round down
				if (cpuQuantity - remainder) < minBound {
					// Nope, can't round down without going below bounds.  Round up.
					cpuQuantity += upDelta
				} else {
					cpuQuantity -= remainder
				}
			} else {
				// Try to round up.
				if (cpuQuantity + upDelta) > maxBound {
					// Nope, can't round up without going above bounds.  Round down .
					cpuQuantity -= remainder
				} else {
					cpuQuantity += upDelta
				}
			}
		}
	}
	result := core.ResourceList{
		core.ResourceCPU:    *resource.NewMilliQuantity(cpuQuantity, resource.DecimalSI),
		core.ResourceMemory: *resource.NewQuantity(reqMem+int64(float64(memDiff)*proportion), resource.DecimalSI),
	}
	return result
}

// GetContainersResources returns the recommended resources for each container in the given pod in the same order they are specified in the pod.Spec.
// If addAll is set to true, containers w/o a recommendation are also added to the list, otherwise they're skipped (default behaviour).  If enableQos
// is set to true, turn QoS Guaranteed (limits=cores) when the VPA annotations are right and the core count is integral.
func GetContainersResources(pod *core.Pod, vpaResourcePolicy *vpa_types.PodResourcePolicy, podRecommendation vpa_types.RecommendedPodResources, limitRange *core.LimitRangeItem,
	addAll bool, annotations vpa_api_util.ContainerToAnnotationsMap, vpaAnnotations map[string]string, enableQos bool) []vpa_api_util.ContainerResources {
	var proportion float64 = float64(*percentageFlag) / 100.0
	if (proportion < 0 || proportion > 1.0) && !percentErrorPrinted {
		klog.Errorf("Invalid value for --percentage.  Treating as 100%.")
		proportion = 1.0
		percentErrorPrinted = true
	}
	resources := make([]vpa_api_util.ContainerResources, len(pod.Spec.Containers))
	inQoS := false
	if enableQos {
		if k8sType, ok := vpaAnnotations[annotationObjectType]; ok {
			if k8sType != "DaemonSet" {
				inQoS = true
			}
		}
		if overrideEnable, ok := vpaAnnotations[annotationQosEnable]; ok {
			switch strings.ToLower(overrideEnable) {
			case "true":
				inQoS = true
			case "false":
				inQoS = false
			default:
				klog.V(2).Info("invalid annotation %s for pod %s/%s: %s, sticking with QoS mode=%v", annotationQosEnable, pod.Namespace, pod.Name, overrideEnable, inQoS)
			}
		}
	}
	for i, container := range pod.Spec.Containers {
		recommendation := vpa_api_util.GetRecommendationForContainer(container.Name, &podRecommendation)
		if recommendation == nil {
			if !addAll {
				klog.V(2).Infof("no matching recommendation found for container %s, skipping", container.Name)
				continue
			}
			klog.V(2).Infof("no matching recommendation found for container %s, using Pod request", container.Name)
			resources[i].Requests = container.Resources.Requests
		} else {
			resources[i].Requests = percentageToTarget(container.Resources.Requests, recommendation.Target, proportion, inQoS)
		}
		defaultLimit := core.ResourceList{}
		if limitRange != nil {
			defaultLimit = limitRange.Default
		}
		containerControlledValues := vpa_api_util.GetContainerControlledValues(container.Name, vpaResourcePolicy)
		if containerControlledValues == vpa_types.ContainerControlledValuesRequestsAndLimits {
			var proportionalLimits core.ResourceList
			var limitAnnotations []string
			if inQoS {
				proportionalLimits, limitAnnotations = vpa_api_util.GetQoSLimit(container.Resources.Limits, container.Resources.Requests, resources[i].Requests, defaultLimit)
			} else {
				proportionalLimits, limitAnnotations = vpa_api_util.GetProportionalLimit(container.Resources.Limits, container.Resources.Requests, resources[i].Requests, defaultLimit)
			}
			if proportionalLimits != nil {
				resources[i].Limits = proportionalLimits
				if len(limitAnnotations) > 0 {
					annotations[container.Name] = append(annotations[container.Name], limitAnnotations...)
				}
			}
		}
	}
	return resources
}

// GetContainersResourcesForPod returns recommended request for a given pod and associated annotations.
// The returned slice corresponds 1-1 to containers in the Pod.
func (p *recommendationProvider) GetContainersResourcesForPod(pod *core.Pod, vpa *vpa_types.VerticalPodAutoscaler) ([]vpa_api_util.ContainerResources, vpa_api_util.ContainerToAnnotationsMap, error) {
	if vpa == nil || pod == nil {
		klog.V(2).Infof("can't calculate recommendations, one of vpa(%+v), pod(%+v) is nil", vpa, pod)
		return nil, nil, nil
	}
	klog.V(2).Infof("updating requirements for pod %s.", pod.Name)

	var annotations vpa_api_util.ContainerToAnnotationsMap
	recommendedPodResources := &vpa_types.RecommendedPodResources{}

	if vpa.Status.Recommendation != nil {
		var err error
		recommendedPodResources, annotations, err = p.recommendationProcessor.Apply(vpa.Status.Recommendation, vpa.Spec.ResourcePolicy, vpa.Status.Conditions, pod)
		if err != nil {
			klog.V(2).Infof("cannot process recommendation for pod %s", pod.Name)
			return nil, annotations, err
		}
	}
	containerLimitRange, err := p.limitsRangeCalculator.GetContainerLimitRangeItem(pod.Namespace)
	if err != nil {
		return nil, nil, fmt.Errorf("error getting containerLimitRange: %s", err)
	}
	var resourcePolicy *vpa_types.PodResourcePolicy
	if vpa.Spec.UpdatePolicy == nil || vpa.Spec.UpdatePolicy.UpdateMode == nil || *vpa.Spec.UpdatePolicy.UpdateMode != vpa_types.UpdateModeOff {
		resourcePolicy = vpa.Spec.ResourcePolicy
	}
	containerResources := GetContainersResources(pod, resourcePolicy, *recommendedPodResources, containerLimitRange, false, annotations, vpa.GetAnnotations(), p.enableQoS)
	return containerResources, annotations, nil
}
