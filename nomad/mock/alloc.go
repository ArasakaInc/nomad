// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package mock

import (
	"math/rand"

	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
)

func Alloc() *structs.Allocation {
	job := Job()
	alloc := &structs.Allocation{
		ID:        uuid.Generate(),
		EvalID:    uuid.Generate(),
		NodeID:    "12345678-abcd-efab-cdef-123456789abc",
		Namespace: structs.DefaultNamespace,
		TaskGroup: "web",
		AllocatedResources: &structs.AllocatedResources{
			Tasks: map[string]*structs.AllocatedTaskResources{
				"web": {
					Cpu: structs.AllocatedCpuResources{
						CpuShares: 500,
					},
					Memory: structs.AllocatedMemoryResources{
						MemoryMB: 256,
					},
					Networks: []*structs.NetworkResource{
						{
							Device:        "eth0",
							IP:            "192.168.0.100",
							ReservedPorts: []structs.Port{{Label: "admin", Value: 5000}},
							MBits:         50,
							DynamicPorts:  []structs.Port{{Label: "http", Value: 9876}},
						},
					},
				},
			},
			Shared: structs.AllocatedSharedResources{
				DiskMB: 150,
			},
		},
		Job:           job,
		DesiredStatus: structs.AllocDesiredStatusRun,
		ClientStatus:  structs.AllocClientStatusPending,
	}
	alloc.JobID = alloc.Job.ID
	alloc.Canonicalize()
	return alloc
}

func AllocWithoutReservedPort() *structs.Allocation {
	alloc := Alloc()
	alloc.AllocatedResources.Tasks["web"].Networks[0].ReservedPorts = nil

	return alloc
}

func AllocForNode(n *structs.Node) *structs.Allocation {
	nodeIP := n.NodeResources.NodeNetworks[0].Addresses[0].Address

	dynamicPortRange := structs.DefaultMaxDynamicPort - structs.DefaultMinDynamicPort
	randomDynamicPort := rand.Intn(dynamicPortRange) + structs.DefaultMinDynamicPort

	alloc := Alloc()
	alloc.NodeID = n.ID

	// Set node IP address.
	alloc.AllocatedResources.Tasks["web"].Networks[0].IP = nodeIP

	// Set dynamic port to a random value.
	alloc.AllocatedResources.Tasks["web"].Networks[0].DynamicPorts = []structs.Port{{Label: "http", Value: randomDynamicPort}}

	return alloc

}

func AllocForNodeWithoutReservedPort(n *structs.Node) *structs.Allocation {
	nodeIP := n.NodeResources.NodeNetworks[0].Addresses[0].Address

	dynamicPortRange := structs.DefaultMaxDynamicPort - structs.DefaultMinDynamicPort
	randomDynamicPort := rand.Intn(dynamicPortRange) + structs.DefaultMinDynamicPort

	alloc := AllocWithoutReservedPort()
	alloc.NodeID = n.ID

	// Set node IP address.
	alloc.AllocatedResources.Tasks["web"].Networks[0].IP = nodeIP

	// Set dynamic port to a random value.
	alloc.AllocatedResources.Tasks["web"].Networks[0].DynamicPorts = []structs.Port{{Label: "http", Value: randomDynamicPort}}

	return alloc
}

func SysBatchAlloc() *structs.Allocation {
	job := SystemBatchJob()
	return &structs.Allocation{
		ID:        uuid.Generate(),
		EvalID:    uuid.Generate(),
		NodeID:    "12345678-abcd-efab-cdef-123456789abc",
		Namespace: structs.DefaultNamespace,
		TaskGroup: "pinger",
		AllocatedResources: &structs.AllocatedResources{
			Tasks: map[string]*structs.AllocatedTaskResources{
				"ping-example": {
					Cpu:    structs.AllocatedCpuResources{CpuShares: 500},
					Memory: structs.AllocatedMemoryResources{MemoryMB: 256},
					Networks: []*structs.NetworkResource{{
						Device: "eth0",
						IP:     "192.168.0.100",
					}},
				},
			},
			Shared: structs.AllocatedSharedResources{DiskMB: 150},
		},
		Job:           job,
		JobID:         job.ID,
		DesiredStatus: structs.AllocDesiredStatusRun,
		ClientStatus:  structs.AllocClientStatusPending,
	}
}

func SystemAlloc() *structs.Allocation {
	alloc := &structs.Allocation{
		ID:        uuid.Generate(),
		EvalID:    uuid.Generate(),
		NodeID:    "12345678-abcd-efab-cdef-123456789abc",
		Namespace: structs.DefaultNamespace,
		TaskGroup: "web",
		AllocatedResources: &structs.AllocatedResources{
			Tasks: map[string]*structs.AllocatedTaskResources{
				"web": {
					Cpu: structs.AllocatedCpuResources{
						CpuShares: 500,
					},
					Memory: structs.AllocatedMemoryResources{
						MemoryMB: 256,
					},
					Networks: []*structs.NetworkResource{
						{
							Device:        "eth0",
							IP:            "192.168.0.100",
							ReservedPorts: []structs.Port{{Label: "admin", Value: 5000}},
							MBits:         50,
							DynamicPorts:  []structs.Port{{Label: "http", Value: 9876}},
						},
					},
				},
			},
			Shared: structs.AllocatedSharedResources{
				DiskMB: 150,
			},
		},
		Job:           SystemJob(),
		DesiredStatus: structs.AllocDesiredStatusRun,
		ClientStatus:  structs.AllocClientStatusPending,
	}
	alloc.JobID = alloc.Job.ID
	return alloc
}
