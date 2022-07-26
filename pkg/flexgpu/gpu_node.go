package flexgpu

import (
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sort"
	"strconv"
)

type gpuNode struct {
	gpuCount    *resource.Quantity
	memoryTotal *resource.Quantity
	gpus        []*gpu
}

type gpu struct {
	index      int
	monopoly   bool
	memory     *resource.Quantity
	usedMemory *resource.Quantity
}

func (u *gpu) String() string {
	return fmt.Sprintf("gpu: { index: %v, monopoly: %v, memory: %v, usedMemory: %v }", u.index, u.monopoly, u.memory, u.usedMemory)
}

func NewGPUNode(nodeInfo *framework.NodeInfo) *gpuNode {

	var pods []*v1.Pod
	for _, po := range nodeInfo.Pods {
		pods = append(pods, po.Pod)
	}

	// checked before construct calling
	gpuAllocatable, _ := nodeInfo.Node().Status.Allocatable[GPUResourceName]
	gpuCnt, ok := gpuAllocatable.AsInt64()
	if !ok {
		panic("invalid gpu resource format")
	}

	// checked before construct calling
	memAllocatable, _ := nodeInfo.Node().Status.Allocatable[MemResourceName]
	memCnt, ok := memAllocatable.AsInt64()
	if !ok {
		panic("invalid memory resource format")
	}

	// assume all gpu in one node is same model.
	// TODO: support heterogeneous gpus in one node.
	// TODO: maybe by let device plugin add annotation to node automatically.
	klog.V(6).InfoS("calculate", "memory", memCnt, "gpu", gpuCnt)
	memEachGPU := resource.NewQuantity(memCnt/gpuCnt, resource.DecimalSI)
	klog.V(6).InfoS("memory each gpu", "memory", memEachGPU.String())

	gpus := constructGPUs(int(gpuCnt), memEachGPU, pods)

	return &gpuNode{
		gpuCount:    &gpuAllocatable,
		memoryTotal: &memAllocatable,
		gpus:        gpus,
	}
}

func constructGPUs(cnt int, memEachGPU *resource.Quantity, pods []*v1.Pod) []*gpu {
	gpus := make([]*gpu, cnt)
	for i := 0; i < cnt; i++ {
		gpus[i] = &gpu{
			index:      i,
			monopoly:   false,
			memory:     memEachGPU,
			usedMemory: resource.NewQuantity(0, resource.DecimalSI),
		}
	}

	for _, pod := range pods {
		gpuLimit, gpuExist := podResourceLimit(GPUResourceName, pod)
		if gpuExist && gpuLimit.CmpInt64(1) != 0 {
			klog.Warningf("pod %s resource %s limit %s invalid", pod.Name, GPUResourceName, gpuLimit.String())
		}
		memLimit, memExist := podResourceLimit(MemResourceName, pod)

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
			gpus[index].monopoly = true
		}

		if memExist {
			klog.V(6).InfoS("add memory", "pod", klog.KObj(pod))
			gpus[index].usedMemory.Add(*memLimit)
		}
	}

	return gpus
}

func (n *gpuNode) MemAssumeFitIndexes(memLimit *resource.Quantity) []int {
	type fit struct {
		index  int
		remain *resource.Quantity
	}

	var fits []fit
	for _, u := range n.gpus {
		if u.monopoly && u.usedMemory.CmpInt64(0) != 0 {
			klog.Warningf("conflict resource %s and %s on gpu index %d", GPUResourceName, MemResourceName, u.index)
		}

		assumed := u.usedMemory
		assumed.Add(*memLimit)
		if !u.monopoly && u.memory.Cmp(*assumed) >= 0 {
			klog.V(6).InfoS("possible fit", "index", u.index)

			remain := u.memory
			remain.Sub(*assumed)
			f := fit{
				index:  u.index,
				remain: remain,
			}

			fits = append(fits, f)
		}
	}

	// sort to perform bin-pack affinity
	// TODO: maybe provide spread affinity
	sort.Slice(fits, func(i, j int) bool {
		return fits[i].remain.Cmp(*fits[j].remain) < 0
	})

	var indexes []int
	for _, f := range fits {
		indexes = append(indexes, f.index)
	}
	return indexes
}

func (n *gpuNode) GPUAssumeFitIndexes(gpuLimit *resource.Quantity) []int {

	var fits []int
	for _, u := range n.gpus {
		if u.monopoly && u.usedMemory.CmpInt64(0) != 0 {
			klog.Warningf("conflict resource %s and %s on gpu index %d", GPUResourceName, MemResourceName, u.index)
		}

		if !u.monopoly && u.usedMemory.IsZero() {
			klog.V(6).InfoS("possible fit", "index", u.index)
			fits = append(fits, u.index)
		}
	}
	return fits
}

func (n *gpuNode) GPUScore() int64 {
	cnt := 0
	for _, u := range n.gpus {
		if !u.monopoly && u.usedMemory.IsZero() {
			cnt++
		}
	}
	return int64(cnt)
}

func (n *gpuNode) MemScore() int64 {
	remainSum := resource.NewQuantity(0, resource.DecimalSI)

	for _, u := range n.gpus {
		remainSum.Add(*u.memory)
		remainSum.Sub(*u.usedMemory)
	}

	rs, _ := remainSum.AsInt64()
	return rs
}
