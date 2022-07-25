package flexgpu

import (
	"context"
	"fmt"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"strconv"
)

const (
	Name                  = "FlexGPU"
	GPUResourceName       = "nvidia.flex.com/gpu"
	MemResourceName       = "nvidia.flex.com/memory"
	GPUIndexAnnotationKey = "nvidia.flex.com/index"
)

type FlexGPU struct {
	handle framework.Handle
}

var _ framework.FilterPlugin = &FlexGPU{}
var _ framework.ScorePlugin = &FlexGPU{}
var _ framework.BindPlugin = &FlexGPU{}
var _ framework.ReservePlugin = &FlexGPU{}

func (f FlexGPU) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(_ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &FlexGPU{handle: h}, nil
}

func (f FlexGPU) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if klog.V(6).Enabled() {
		klog.InfoS("node info", "point", "filter", "name", nodeInfo.Node().Name)
		klog.InfoS("pod info", "point", "filter", "name", pod.Name)
		for _, container := range pod.Spec.Containers {
			for k, v := range container.Resources.Limits {
				klog.InfoS("resource limit", "container", container.Name, k, v.AsDec().String())
			}
		}
	}

	gpuLimit, gpuLimitExist := podResourceLimit(GPUResourceName, pod)
	memLimit, memLimitExist := podResourceLimit(MemResourceName, pod)
	if !gpuLimitExist && !memLimitExist {
		return nil
	}

	if gpuLimitExist && memLimitExist {
		klog.Warningf("pod conflict resources %s and %s", GPUResourceName, MemResourceName)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "pod conflict resources")
	}

	// return if unknown resource type
	gpuAllocatable, ok := nodeInfo.Node().Status.Allocatable[GPUResourceName]
	if !ok {
		klog.V(6).InfoS("unknown", "resource", GPUResourceName)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "unknown resource type")
	}

	memAllocatable, ok := nodeInfo.Node().Status.Allocatable[MemResourceName]
	if !ok {
		klog.V(6).InfoS("unknown", "resource", MemResourceName)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "unknown resource type")
	}

	// calculate assume quantity, return if insufficient
	var pods []*v1.Pod
	for _, po := range nodeInfo.Pods {
		pods = append(pods, po.Pod)
	}

	gpuCurrLimitSum := nodeResourceLimitSum(GPUResourceName, nodeInfo)
	memCurrLimitSum := nodeResourceLimitSum(MemResourceName, nodeInfo)

	gpuAssumeLimitSum := gpuCurrLimitSum
	memAssumeLimitSum := memCurrLimitSum
	gpuAssumeLimitSum.Add(*gpuLimit)
	memAssumeLimitSum.Add(*memLimit)

	if gpuAssumeLimitSum.Cmp(gpuAllocatable) > 0 {
		klog.V(6).InfoS("insufficient", "resource", GPUResourceName, "assume", gpuAssumeLimitSum.String(), "allocatable", gpuAllocatable.String())
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("insufficient resource %v", GPUResourceName))
	}

	if memAssumeLimitSum.Cmp(memAllocatable) > 0 {
		klog.V(6).InfoS("insufficient", "resource", MemResourceName, "assume", memAssumeLimitSum.String(), "allocatable", memAllocatable.String())
		return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("insufficient resource %v", MemResourceName))
	}

	// filter out possible fit gpus
	no := NewGPUNode(nodeInfo)

	if klog.V(6).Enabled() {
		for _, u := range no.gpus {
			klog.InfoS("node gpu usages", "node", klog.KObj(nodeInfo.Node()), "usage", u.String())
		}
	}

	if memLimitExist {
		indexes := no.MemAssumeFitIndexes(memLimit)

		klog.V(6).InfoS("fit indexes", "indexes", indexes)
		if len(indexes) == 0 {
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("no fit indexes resource %v", MemResourceName))
		}
	}

	return nil
}

func podResourceLimit(name v1.ResourceName, pod *v1.Pod) (*resource.Quantity, bool) {
	exist := false
	resourceLimitSum := resource.NewQuantity(0, resource.DecimalSI)
	for _, container := range pod.Spec.Containers {
		if q, ok := container.Resources.Limits[name]; ok {
			exist = true
			resourceLimitSum.Add(q)
		}
	}
	return resourceLimitSum, exist
}

func nodeResourceLimitSum(name v1.ResourceName, nodeInfo *framework.NodeInfo) *resource.Quantity {
	resourceLimitSum := resource.NewQuantity(0, resource.DecimalSI)
	for _, podInfo := range nodeInfo.Pods {
		limit, _ := podResourceLimit(name, podInfo.Pod)
		resourceLimitSum.Add(*limit)
	}
	return resourceLimitSum
}

func (f FlexGPU) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	for _, container := range pod.Spec.Containers {
		for k, v := range container.Resources.Limits {
			klog.V(6).InfoS("resource limit", "point", "score", k, v.AsDec().String())
		}
	}
	return 0, nil
}

func (f FlexGPU) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

func (f FlexGPU) Reserve(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	nodeInfo, err := f.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
	if err != nil {
		return framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q from Snapshot: %v", nodeName, err))
	}

	gpuLimit, gpuLimitExist := podResourceLimit(GPUResourceName, p)
	memLimit, memLimitExist := podResourceLimit(MemResourceName, p)

	if !gpuLimitExist && !memLimitExist {
		return nil
	}

	if gpuLimitExist && memLimitExist {
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "pod conflict resources")
	}

	no := NewGPUNode(nodeInfo)

	if gpuLimitExist {
		indexes := no.GPUAssumeFitIndexes(gpuLimit)
		if len(indexes) == 0 {
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("allocate index fail %v", GPUResourceName))
		}
		if p.Annotations == nil {
			p.Annotations = make(map[string]string)
		}
		p.Annotations[GPUIndexAnnotationKey] = strconv.Itoa(indexes[0])
	}

	if memLimitExist {
		indexes := no.MemAssumeFitIndexes(memLimit)
		if len(indexes) == 0 {
			return framework.NewStatus(framework.Unschedulable, fmt.Sprintf("allocate index fail %v", MemResourceName))
		}
		if p.Annotations == nil {
			p.Annotations = make(map[string]string)
		}
		// indexes is sorted by bin-pack affinity
		// TODO: maybe provide spread affinity
		p.Annotations[GPUIndexAnnotationKey] = strconv.Itoa(indexes[0])
	}

	klog.V(6).InfoS("annotations", "annotations", p.Annotations)
	return nil
}

func (f FlexGPU) Unreserve(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) {
	klog.V(6).InfoS("unreserve", "annotations", p.Annotations)
	delete(p.Annotations, GPUIndexAnnotationKey)
}

func (f FlexGPU) Bind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	klog.V(6).InfoS("annotations", "annotations", p.Annotations)
	klog.V(3).InfoS("Attempting to bind pod to node", "pod", klog.KObj(p), "node", klog.KRef("", nodeName))
	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name, UID: p.UID, Annotations: p.Annotations},
		Target:     v1.ObjectReference{Kind: "Node", Name: nodeName},
	}
	err := f.handle.ClientSet().CoreV1().Pods(binding.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
	if err != nil {
		return framework.AsStatus(err)
	}
	return nil
}
