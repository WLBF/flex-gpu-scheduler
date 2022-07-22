package flexgpu

import (
	"context"
	"fmt"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

	gpuLimit, gpuLimitExist := podResourceLimitSum(GPUResourceName, pod)
	memLimit, memLimitExist := podResourceLimitSum(MemResourceName, pod)
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
	gpuCnt, ok := gpuAllocatable.AsInt64()
	if !ok {
		panic("invalid gpu resource format")
	}

	memAllocatable, ok := nodeInfo.Node().Status.Allocatable[MemResourceName]
	memCnt, ok := memAllocatable.AsInt64()
	if !ok {
		panic("invalid memory resource format")
	}
	if !ok {
		klog.V(6).InfoS("unknown", "resource", MemResourceName)
		return framework.NewStatus(framework.UnschedulableAndUnresolvable, "unknown resource type")
	}

	klog.V(6).InfoS("calculate", "memory", memCnt, "gpu", gpuCnt)
	memEachGPU := resource.NewQuantity(memCnt/gpuCnt, resource.DecimalSI)
	klog.V(6).InfoS("memory each gpu", "memory", memEachGPU.String())

	// calculate assume quantity, return if insufficient
	var pods []*v1.Pod
	for _, po := range nodeInfo.Pods {
		pods = append(pods, po.Pod)
	}

	gpuCurrLimitSum := podsResourceLimitSum(GPUResourceName, pods)
	memCurrLimitSum := podsResourceLimitSum(MemResourceName, pods)

	gpuAssumeLimitSum := gpuCurrLimitSum
	memAssumeLimitSum := memCurrLimitSum
	gpuAssumeLimitSum.Add(*gpuLimit)
	memAssumeLimitSum.Add(*memLimit)

	if gpuAssumeLimitSum.Cmp(gpuAllocatable) > 0 {
		klog.V(6).InfoS("insufficient", "resource", GPUResourceName, "assume", gpuAssumeLimitSum.String(), "allocatable", gpuAllocatable.String())
		return framework.NewStatus(framework.Unschedulable, "insufficient resource")
	}

	if memAssumeLimitSum.Cmp(memAllocatable) > 0 {
		klog.V(6).InfoS("insufficient", "resource", MemResourceName, "assume", memAssumeLimitSum.String(), "allocatable", memAllocatable.String())
		return framework.NewStatus(framework.Unschedulable, "insufficient resource")
	}

	// filter out possible fit gpus
	usage := resourceLimitSumIndexArray(int(gpuCnt), pods)
	if klog.V(6).Enabled() {
		for _, u := range usage {
			klog.Infoln(u.String())
		}
	}

	fit := 0
	for _, u := range usage {
		if u.monopoly && u.memory.CmpInt64(0) != 0 {
			klog.Warningf("conflict resource %s and %s on gpu index %d", GPUResourceName, MemResourceName, u.index)
		}

		u.memory.Add(*memLimit)
		if !u.monopoly && memEachGPU.Cmp(*u.memory) >= 0 {
			klog.V(6).InfoS("possible fit", "index", u.index)
			fit++
		}
	}

	klog.V(6).InfoS("fit nodes", "count", fit)
	if fit == 0 {
		return framework.NewStatus(framework.Unschedulable, "insufficient resource")
	}

	return nil
}

func podResourceLimitSum(name v1.ResourceName, pod *v1.Pod) (*resource.Quantity, bool) {
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

func podsResourceLimitSum(name v1.ResourceName, pods []*v1.Pod) *resource.Quantity {
	resourceLimitSum := resource.NewQuantity(0, resource.DecimalSI)
	for _, pod := range pods {
		limit, _ := podResourceLimitSum(name, pod)
		resourceLimitSum.Add(*limit)
	}
	return resourceLimitSum
}

type gpuUsage struct {
	index    int
	monopoly bool
	memory   *resource.Quantity
}

func (u *gpuUsage) String() string {
	return fmt.Sprintf("usage: { index: %v, monopoly: %v, memory: %v}", u.index, u.monopoly, u.memory)
}

func resourceLimitSumIndexArray(cnt int, pods []*v1.Pod) []*gpuUsage {
	usage := make([]*gpuUsage, cnt)
	for i := 0; i < cnt; i++ {
		usage[i] = &gpuUsage{
			index:    i,
			monopoly: false,
			memory:   resource.NewQuantity(0, resource.DecimalSI),
		}
	}

	for _, pod := range pods {
		gpuLimit, gpuExist := podResourceLimitSum(GPUResourceName, pod)
		if gpuExist && gpuLimit.CmpInt64(1) != 0 {
			klog.Warningf("pod %s resource %s limit %s invalid", pod.Name, GPUResourceName, gpuLimit.String())
		}
		memLimit, memExist := podResourceLimitSum(MemResourceName, pod)

		if !gpuExist && !memExist {
			klog.V(6).InfoS("skip", "pod", klog.KObj(pod))
			continue
		}

		klog.V(6).InfoS("set monopoly", "pod", klog.KObj(pod))
		val, hasAnnotation := pod.ObjectMeta.Annotations[GPUIndexAnnotationKey]
		index, err := strconv.Atoi(val)
		if err != nil {
			klog.Warningf("pod %s invalid index annotation %s", pod.Name, val)
			continue
		}

		if !hasAnnotation {
			if !gpuLimit.IsZero() {
				klog.Warningf("pod %s no index annotation with %s limit set", pod.Name, GPUResourceName)
			}
			if !memLimit.IsZero() {
				klog.Warningf("pod %s no index annotation with %s limit set", pod.Name, MemResourceName)
			}
			continue
		}

		if gpuExist {
			klog.V(6).InfoS("set monopoly", "pod", klog.KObj(pod))
			usage[index].monopoly = true
		}

		if memExist {
			klog.V(6).InfoS("add memory", "pod", klog.KObj(pod))
			usage[index].memory.Add(*memLimit)
		}
	}

	return usage
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

func (f FlexGPU) Bind(ctx context.Context, state *framework.CycleState, p *v1.Pod, nodeName string) *framework.Status {
	//TODO implement me
	panic("implement me")
}
