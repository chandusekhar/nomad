package nomad

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/hashicorp/nomad/testutil/rpcapi"
	"github.com/stretchr/testify/require"
)

// TestNodeDrainer_SimpleDrain asserts that draining when there are two nodes
// moves allocs from the draining node to the other node.
func TestNodeDrainer_SimpleDrain(t *testing.T) {
	require := require.New(t)
	server := TestServer(t, nil)
	defer server.Shutdown()

	testutil.WaitForLeader(t, server.RPC)

	// Setup 2 Nodes: A & B; A has allocs and is draining

	// Create mock jobs
	state := server.fsm.State()

	serviceJob := mock.Job()
	serviceJob.Name = "service-job"
	serviceJob.Type = structs.JobTypeService
	serviceJob.TaskGroups[0].Migrate = &structs.MigrateStrategy{
		MaxParallel:     1,
		HealthCheck:     structs.MigrateStrategyHealthStates,
		MinHealthyTime:  time.Millisecond,
		HealthyDeadline: 2 * time.Second,
	}
	serviceJob.TaskGroups[0].Tasks[0].Driver = "mock_driver"
	serviceJob.TaskGroups[0].Tasks[0].Resources = structs.MinResources()
	serviceJob.TaskGroups[0].Tasks[0].Config = map[string]interface{}{
		"run_for":    "10m",
		"kill_after": "1ms",
	}
	serviceJob.TaskGroups[0].Tasks[0].Services = nil

	systemJob := mock.SystemJob()
	systemJob.Name = "system-job"
	systemJob.Type = structs.JobTypeSystem
	//FIXME hack until system job reschedule policy validation is fixed
	systemJob.TaskGroups[0].ReschedulePolicy = &structs.ReschedulePolicy{Attempts: 1, Interval: time.Minute}
	systemJob.TaskGroups[0].Tasks[0].Driver = "mock_driver"
	systemJob.TaskGroups[0].Tasks[0].Config = map[string]interface{}{
		"run_for":    "10m",
		"kill_after": "1ms",
	}
	systemJob.TaskGroups[0].Tasks[0].Resources = structs.MinResources()
	systemJob.TaskGroups[0].Tasks[0].Services = nil

	batchJob := mock.Job()
	batchJob.Name = "batch-job"
	batchJob.Type = structs.JobTypeBatch
	batchJob.TaskGroups[0].Name = "batch-group"
	batchJob.TaskGroups[0].Migrate = nil
	batchJob.TaskGroups[0].Tasks[0].Name = "batch-task"
	batchJob.TaskGroups[0].Tasks[0].Driver = "mock_driver"
	batchJob.TaskGroups[0].Tasks[0].Config = map[string]interface{}{
		"run_for":    "10m",
		"kill_after": "1ms",
		"exit_code":  13, // set nonzero exit code to cause rescheduling
	}
	batchJob.TaskGroups[0].Tasks[0].Resources = structs.MinResources()
	batchJob.TaskGroups[0].Tasks[0].Services = nil

	// Start node 1
	c1 := client.TestClient(t, func(conf *config.Config) {
		conf.LogOutput = testlog.NewWriter(t)
		conf.Servers = []string{server.config.RPCAddr.String()}
	})
	defer c1.Shutdown()

	// Start jobs so they all get placed on node 1
	codec := rpcClient(t, server)
	for _, job := range []*structs.Job{systemJob, serviceJob, batchJob} {
		req := &structs.JobRegisterRequest{
			Job: job.Copy(),
			WriteRequest: structs.WriteRequest{
				Region:    "global",
				Namespace: job.Namespace,
			},
		}

		// Fetch the response
		var resp structs.JobRegisterResponse
		require.Nil(msgpackrpc.CallWithCodec(codec, "Job.Register", req, &resp))
		require.NotZero(resp.Index)
	}

	// Wait for jobs to start on c1
	rpc := rpcapi.NewRPC(codec)
	testutil.WaitForResult(func() (bool, error) {
		resp, err := rpc.NodeGetAllocs(c1.NodeID())
		if err != nil {
			return false, err
		}

		system, batch, service := 0, 0, 0
		for _, alloc := range resp.Allocs {
			if alloc.ClientStatus != structs.AllocClientStatusRunning {
				return false, fmt.Errorf("alloc %s for job %s not running: %s", alloc.ID, alloc.Job.Name, alloc.ClientStatus)
			}
			switch alloc.JobID {
			case batchJob.ID:
				batch++
			case serviceJob.ID:
				service++
			case systemJob.ID:
				system++
			}
		}
		// 1 system + 10 batch + 10 service = 21
		if system+batch+service != 21 {
			return false, fmt.Errorf("wrong number of allocs: system %d/1, batch %d/10, service %d/10", system, batch, service)
		}
		return true, nil
	}, func(err error) {
		if resp, err := rpc.NodeGetAllocs(c1.NodeID()); err == nil {
			for i, alloc := range resp.Allocs {
				t.Logf("%d alloc %s job %s status %s", i, alloc.ID, alloc.Job.Name, alloc.ClientStatus)
			}
		}
		t.Fatalf("failed waiting for all allocs to start: %v", err)
	})

	// Start draining node 1
	//FIXME update drain rpc to skip fsm manipulation and use api
	node, err := state.NodeByID(nil, c1.NodeID())
	require.Nil(err)
	require.Nil(state.UpdateNodeDrain(node.ModifyIndex+1, node.ID, true))

	// Start node 2
	c2 := client.TestClient(t, func(conf *config.Config) {
		conf.NetworkSpeed = 10000
		conf.Servers = []string{server.config.RPCAddr.String()}
	})
	defer c2.Shutdown()

	// Wait for services to be migrated
	testutil.WaitForResult(func() (bool, error) {
		resp, err := rpc.NodeGetAllocs(c2.NodeID())
		if err != nil {
			return false, err
		}

		system, batch, service := 0, 0, 0
		for _, alloc := range resp.Allocs {
			if alloc.ClientStatus != structs.AllocClientStatusRunning {
				return false, fmt.Errorf("alloc %s for job %s not running: %s", alloc.ID, alloc.Job.Name, alloc.ClientStatus)
			}
			switch alloc.JobID {
			case batchJob.ID:
				batch++
			case serviceJob.ID:
				service++
			case systemJob.ID:
				system++
			}
		}
		// 1 system + 10 batch + 10 service = 21
		if system+batch+service != 21 {
			return false, fmt.Errorf("wrong number of allocs: system %d/1, batch %d/10, service %d/10", system, batch, service)
		}
		return true, nil
	}, func(err error) {
		if resp, err := rpc.NodeGetAllocs(c2.NodeID()); err == nil {
			for i, alloc := range resp.Allocs {
				t.Logf("%d alloc %s job %s status %s", i, alloc.ID, alloc.Job.Name, alloc.ClientStatus)
			}
		}
		t.Fatalf("failed waiting for all allocs to start: %v", err)
	})

	// Wait for all service allocs to be replaced
	jobs, err := rpc.JobList()
	require.Nil(err)
	t.Logf("%d jobs", len(jobs.Jobs))
	for _, job := range jobs.Jobs {
		t.Logf("job: %s status: %s %s", job.Name, job.Status, job.StatusDescription)
	}

	allocs, err := rpc.AllocAll()
	require.Nil(err)

	sort.Slice(allocs, func(i, j int) bool {
		r := strings.Compare(allocs[i].Job.Name, allocs[j].Job.Name)
		switch {
		case r < 0:
			return true
		case r == 0:
			return allocs[i].ModifyIndex < allocs[j].ModifyIndex
		case r > 0:
			return false
		}
		panic("unreachable")
	})

	t.Logf("%d allocs", len(allocs))
	for _, alloc := range allocs {
		t.Logf("job: %s node: %s alloc: %s desired: %s actual: %s replaces: %s", alloc.Job.Name, alloc.NodeID[:6], alloc.ID, alloc.DesiredStatus, alloc.ClientStatus, alloc.PreviousAllocation)
	}
}
