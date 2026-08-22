package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/qimaoww/qcontrolhub/internal/authn"
	"github.com/qimaoww/qcontrolhub/internal/core"
	"github.com/qimaoww/qcontrolhub/internal/store"
)

func TestWSSAgentLifecycleWithPostgreSQL(t *testing.T) {
	databaseURL := os.Getenv("QCH_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("QCH_TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dataStore, err := store.Open(ctx, databaseURL, true)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	defer dataStore.Close()

	adminToken := strings.Repeat("a", 48)
	trustedProxies, err := authn.ParseTrustedProxies([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatalf("parse trusted proxy fixture: %v", err)
	}
	httpServer := httptest.NewServer(New(dataStore, Config{
		AdminToken:     adminToken,
		TrustedProxies: trustedProxies,
		AgentBinary:    []byte("test-agent-binary"),
		AgentVersion:   "test-version",
	}).Handler())
	defer httpServer.Close()

	enrollment, err := dataStore.CreateEnrollmentToken(ctx, core.EnrollmentTokenRequest{Name: "integration", TTLMinutes: 5, MaxUses: 1})
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}
	var enrolledAgentID string
	t.Cleanup(func() {
		var agentIDs []string
		if enrolledAgentID != "" {
			agentIDs = append(agentIDs, enrolledAgentID)
		}
		cleanupTaskAPIFixture(t, databaseURL, enrollment.ID, agentIDs)
	})
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	enrollmentBody, _ := json.Marshal(core.EnrollRequest{
		Name: "integration-agent", OS: "linux", Arch: "amd64",
		Capabilities: []core.Engine{core.EngineMihomo},
		Features:     []string{core.AgentFeatureSelfUpgrade, core.AgentFeaturePortTraffic},
		PublicKey:    authn.EncodePublicKey(publicKey),
	})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/agent/v1/enroll", bytes.NewReader(enrollmentBody))
	request.Header.Set("Authorization", "Bearer "+enrollment.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("enroll request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("enroll status = %s", response.Status)
	}
	var enrolled core.EnrollResponse
	if err := json.NewDecoder(response.Body).Decode(&enrolled); err != nil {
		t.Fatalf("decode enrollment: %v", err)
	}
	enrolledAgentID = enrolled.AgentID
	trafficPolicy, err := dataStore.CreatePortTrafficPolicy(ctx, core.PortTrafficPolicyRequest{
		AgentID: enrolled.AgentID, Name: "integration port", Engine: core.EngineMihomo,
		Port: 24443, Protocol: core.TrafficProtocolBoth, Cycle: core.TrafficCycleMonthly,
		CycleAnchor: core.UTCDate(time.Now().UTC()), LimitBytes: 100 << 30,
	})
	if err != nil {
		t.Fatalf("create traffic policy: %v", err)
	}

	// An enrolled Agent can fetch the control-plane's exact binary only with
	// its own fresh Ed25519 request signature. The response is checksummed so
	// the Agent can verify it before replacing its executable.
	binaryRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/agent/v1/binary", nil)
	if err != nil {
		t.Fatalf("create signed binary request: %v", err)
	}
	if err := authn.SignRequest(binaryRequest, nil, enrolled.AgentID, privateKey, time.Now().UTC()); err != nil {
		t.Fatalf("sign binary request: %v", err)
	}
	binaryResponse, err := http.DefaultClient.Do(binaryRequest)
	if err != nil {
		t.Fatalf("download signed Agent binary: %v", err)
	}
	defer binaryResponse.Body.Close()
	if binaryResponse.StatusCode != http.StatusOK {
		t.Fatalf("signed Agent binary status = %s", binaryResponse.Status)
	}
	binaryContents, err := io.ReadAll(binaryResponse.Body)
	if err != nil {
		t.Fatalf("read signed Agent binary: %v", err)
	}
	if string(binaryContents) != "test-agent-binary" || binaryResponse.Header.Get("X-QControlHub-Agent-Version") != "test-version" || binaryResponse.Header.Get("X-QControlHub-Agent-SHA256") == "" {
		t.Fatalf("signed Agent binary response = body %q headers version=%q checksum=%q", string(binaryContents), binaryResponse.Header.Get("X-QControlHub-Agent-Version"), binaryResponse.Header.Get("X-QControlHub-Agent-SHA256"))
	}

	websocketURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/agent/v1/connect"
	handshake, _ := http.NewRequestWithContext(ctx, http.MethodGet, websocketURL, nil)
	if err := authn.SignRequest(handshake, nil, enrolled.AgentID, privateKey, time.Now().UTC()); err != nil {
		t.Fatalf("sign WSS handshake: %v", err)
	}
	handshake.Header.Set("X-Forwarded-For", "2606:4700:4700:0:0:0:0:1111")
	connection, dialResponse, err := websocket.Dial(ctx, websocketURL, &websocket.DialOptions{
		HTTPHeader: handshake.Header, Subprotocols: []string{"qcontrolhub.agent.v1"},
	})
	if err != nil {
		if dialResponse != nil {
			t.Fatalf("dial WSS: %v (%s)", err, dialResponse.Status)
		}
		t.Fatalf("dial WSS: %v", err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	var hello core.WireMessage
	if err := wsjson.Read(ctx, connection, &hello); err != nil || hello.Type != core.WireHello {
		t.Fatalf("read hello: message=%+v error=%v", hello, err)
	}
	if len(hello.TrafficPolicies) != 1 || hello.TrafficPolicies[0].ID != trafficPolicy.ID {
		t.Fatalf("hello traffic policies = %+v", hello.TrafficPolicies)
	}
	connectedAgent, err := dataStore.GetAgent(ctx, enrolled.AgentID)
	if err != nil || connectedAgent.Metrics.ObservedPublicIP != "2606:4700:4700::1111" {
		t.Fatalf("trusted WSS public source was not normalized and stored: agent=%+v error=%v", connectedAgent, err)
	}
	periodStart, periodEnd, err := core.TrafficPeriodAt(trafficPolicy.CycleAnchor, trafficPolicy.Cycle, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := core.WireMessage{Type: core.WireHeartbeat, Heartbeat: &core.HeartbeatRequest{
		Version: "test", Features: []string{core.AgentFeatureSelfUpgrade, core.AgentFeaturePortTraffic, core.AgentFeatureCoreLogs},
		TrafficUsage: []core.PortTrafficUsage{{
			PolicyID: trafficPolicy.ID, ResetGeneration: trafficPolicy.ResetGeneration,
			ReceivedBytes: 2048, SentBytes: 1024, UsedBytes: 3072,
			ReceiveBPS: 128, SendBPS: 64, PeriodStart: periodStart, PeriodEnd: periodEnd,
			EnforcementAvailable: true,
		}}, Metrics: &core.HostMetrics{
			CPUAvailable: true, CPUPercent: 12.5,
			MemoryAvailable: true, MemoryUsedBytes: 2 << 30, MemoryTotalBytes: 4 << 30,
			DiskAvailable: true, DiskUsedBytes: 8 << 30, DiskTotalBytes: 16 << 30,
			NetworkAvailable: true, NetworkRXBytes: 1000, NetworkTXBytes: 500, NetworkRXBPS: 100, NetworkTXBPS: 50,
			NetworkInterfaces: []core.HostNetworkInterface{{Name: "eth0", Addresses: []string{"192.0.2.20"}}},
			ObservedPublicIP:  "203.0.113.99",
		},
	}}
	if err := wsjson.Write(ctx, connection, heartbeat); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	for attempt := 0; attempt < 50; attempt++ {
		policies, listErr := dataStore.AgentPortTrafficPolicies(ctx, enrolled.AgentID)
		if listErr == nil && len(policies) == 1 && policies[0].UsedBytes == 3072 && policies[0].EnforcementAvailable {
			break
		}
		if attempt == 49 {
			t.Fatalf("traffic heartbeat was not stored: policies=%+v error=%v", policies, listErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A high-frequency metrics-only push must refresh the live snapshot
	// without clobbering version, runtime, or features from the heartbeat.
	metricsPush := core.WireMessage{Type: core.WireMetrics, Metrics: &core.HostMetrics{
		CPUAvailable: true, CPUPercent: 42.5,
		MemoryAvailable: true, MemoryUsedBytes: 1 << 30, MemoryTotalBytes: 4 << 30,
		DiskAvailable: true, DiskUsedBytes: 2 << 30, DiskTotalBytes: 16 << 30,
		NetworkAvailable: true, NetworkRXBytes: 2000, NetworkTXBytes: 900, NetworkRXBPS: 300, NetworkTXBPS: 120,
		ObservedPublicIP: "10.0.0.8",
	}}
	if err := wsjson.Write(ctx, connection, metricsPush); err != nil {
		t.Fatalf("write metrics push: %v", err)
	}
	for attempt := 0; attempt < 50; attempt++ {
		pushed, pushErr := dataStore.GetAgent(ctx, enrolled.AgentID)
		if pushErr == nil && pushed.Metrics.CPUPercent == 42.5 && pushed.Metrics.NetworkRXBPS == 300 {
			if pushed.Version != "test" || len(pushed.Features) == 0 {
				t.Fatalf("metrics push clobbered heartbeat state: agent=%+v", pushed)
			}
			if pushed.Metrics.ObservedPublicIP != "2606:4700:4700::1111" || len(pushed.Metrics.NetworkInterfaces) != 1 || pushed.Metrics.NetworkInterfaces[0].Addresses[0] != "192.0.2.20" {
				t.Fatalf("metrics push did not preserve server-observed and last usable address state: %+v", pushed.Metrics)
			}
			break
		}
		if attempt == 49 {
			t.Fatalf("metrics push was not stored: agent=%+v error=%v", pushed, pushErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	logBatch := core.CoreLogBatch{ID: "log_0123456789abcdef", Entries: []core.CoreLogEntry{{
		Engine: core.EngineMihomo, Level: "info", Message: "integration core log", LoggedAt: time.Now().UTC(),
	}}}
	if err := wsjson.Write(ctx, connection, core.WireMessage{Type: core.WireCoreLogs, CoreLogs: &logBatch}); err != nil {
		t.Fatalf("write core log batch: %v", err)
	}
	var logAcknowledgment core.WireMessage
	if err := wsjson.Read(ctx, connection, &logAcknowledgment); err != nil || logAcknowledgment.Type != core.WireCoreLogsAck || logAcknowledgment.BatchID != logBatch.ID {
		t.Fatalf("core log acknowledgment = %+v, %v", logAcknowledgment, err)
	}
	storedLogs, err := dataStore.ListCoreLogs(ctx, store.CoreLogQuery{AgentID: enrolled.AgentID, Limit: 10})
	if err != nil || len(storedLogs) != 1 || storedLogs[0].Message != logBatch.Entries[0].Message {
		t.Fatalf("stored core logs = %+v, %v", storedLogs, err)
	}
	largeBatch := core.CoreLogBatch{ID: "log_fedcba9876543210", Entries: make([]core.CoreLogEntry, core.MaxCoreLogBatchEntries)}
	for index := range largeBatch.Entries {
		largeBatch.Entries[index] = core.CoreLogEntry{
			Engine: core.EngineXray, Level: "debug", Message: strings.Repeat("\x01", core.MaxCoreLogMessageBytes), LoggedAt: time.Now().UTC(),
		}
	}
	if err := wsjson.Write(ctx, connection, core.WireMessage{Type: core.WireCoreLogs, CoreLogs: &largeBatch}); err != nil {
		t.Fatalf("write maximum encoded core log batch: %v", err)
	}
	if err := wsjson.Read(ctx, connection, &logAcknowledgment); err != nil || logAcknowledgment.Type != core.WireCoreLogsAck || logAcknowledgment.BatchID != largeBatch.ID {
		t.Fatalf("maximum core log acknowledgment = %+v, %v", logAcknowledgment, err)
	}

	config, err := dataStore.SaveAgentConfig(ctx, core.Config{
		AgentID: enrolled.AgentID, Name: "integration node configuration", Engine: core.EngineMihomo,
		Content: "mixed-port: 7890\nproxies: []\nproxy-groups: []\nrules:\n  - MATCH,DIRECT\n",
	}, 0)
	if err != nil {
		t.Fatalf("create agent config: %v", err)
	}
	queued, err := dataStore.CreateTask(ctx, core.TaskRequest{
		AgentID: enrolled.AgentID, Action: core.ActionValidate, Engine: core.EngineMihomo, ConfigID: config.ID,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	var taskMessage core.WireMessage
	if err := wsjson.Read(ctx, connection, &taskMessage); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if taskMessage.Type != core.WireTask || taskMessage.Task == nil || taskMessage.Task.ID != queued.ID || taskMessage.Task.LeaseID == "" {
		t.Fatalf("invalid task message: %+v", taskMessage)
	}
	result := core.WireMessage{Type: core.WireResult, Result: &core.TaskResultEnvelope{
		TaskID: queued.ID,
		Result: core.TaskResultRequest{LeaseID: taskMessage.Task.LeaseID, Success: true, Output: "validated"},
	}}
	if err := wsjson.Write(ctx, connection, result); err != nil {
		t.Fatalf("write result: %v", err)
	}
	var acknowledgment core.WireMessage
	if err := wsjson.Read(ctx, connection, &acknowledgment); err != nil {
		t.Fatalf("read result acknowledgment: %v", err)
	}
	if acknowledgment.Type != core.WireResultAck || acknowledgment.TaskID != queued.ID {
		t.Fatalf("invalid result acknowledgment: %+v", acknowledgment)
	}

	resumable, err := dataStore.CreateTask(ctx, core.TaskRequest{
		AgentID: enrolled.AgentID, Action: core.ActionStatus, Engine: core.EngineMihomo,
	})
	if err != nil {
		t.Fatalf("create resumable task: %v", err)
	}
	var originalLease core.WireMessage
	if err := wsjson.Read(ctx, connection, &originalLease); err != nil || originalLease.Task == nil || originalLease.Task.ID != resumable.ID {
		t.Fatalf("read resumable task: message=%+v error=%v", originalLease, err)
	}
	if err := connection.Close(websocket.StatusGoingAway, "simulate transient disconnect"); err != nil {
		t.Fatalf("close first WSS session: %v", err)
	}

	resumedHandshake, _ := http.NewRequestWithContext(ctx, http.MethodGet, websocketURL, nil)
	if err := authn.SignRequest(resumedHandshake, nil, enrolled.AgentID, privateKey, time.Now().UTC()); err != nil {
		t.Fatalf("sign resumed WSS handshake: %v", err)
	}
	resumedHandshake.Header.Set("X-Forwarded-For", "93.184.216.34")
	resumedConnection, resumedResponse, err := websocket.Dial(ctx, websocketURL, &websocket.DialOptions{
		HTTPHeader: resumedHandshake.Header, Subprotocols: []string{"qcontrolhub.agent.v1"},
	})
	if err != nil {
		if resumedResponse != nil {
			t.Fatalf("dial resumed WSS: %v (%s)", err, resumedResponse.Status)
		}
		t.Fatalf("dial resumed WSS: %v", err)
	}
	defer resumedConnection.Close(websocket.StatusNormalClosure, "test complete")
	var resumedHello core.WireMessage
	if err := wsjson.Read(ctx, resumedConnection, &resumedHello); err != nil || resumedHello.Type != core.WireHello {
		t.Fatalf("read resumed hello: message=%+v error=%v", resumedHello, err)
	}
	reconnectedAgent, err := dataStore.GetAgent(ctx, enrolled.AgentID)
	if err != nil || reconnectedAgent.Metrics.ObservedPublicIP != "93.184.216.34" {
		t.Fatalf("reconnected WSS public source did not replace the previous observation: agent=%+v error=%v", reconnectedAgent, err)
	}
	var resumedTask core.WireMessage
	if err := wsjson.Read(ctx, resumedConnection, &resumedTask); err != nil {
		t.Fatalf("read resumed task without stale-lease delay: %v", err)
	}
	if resumedTask.Task == nil || resumedTask.Task.ID != resumable.ID || resumedTask.Task.LeaseID != originalLease.Task.LeaseID || resumedTask.Task.Attempt != originalLease.Task.Attempt {
		t.Fatalf("resumed task changed lease or attempt: original=%+v resumed=%+v", originalLease.Task, resumedTask.Task)
	}
	resumedResult := core.WireMessage{Type: core.WireResult, Result: &core.TaskResultEnvelope{
		TaskID: resumable.ID,
		Result: core.TaskResultRequest{LeaseID: resumedTask.Task.LeaseID, Success: true, Output: "resumed status"},
	}}
	if err := wsjson.Write(ctx, resumedConnection, resumedResult); err != nil {
		t.Fatalf("write resumed result: %v", err)
	}
	var resumedAcknowledgment core.WireMessage
	if err := wsjson.Read(ctx, resumedConnection, &resumedAcknowledgment); err != nil || resumedAcknowledgment.TaskID != resumable.ID {
		t.Fatalf("read resumed acknowledgment: message=%+v error=%v", resumedAcknowledgment, err)
	}
	completedResumed, err := dataStore.GetTask(ctx, resumable.ID)
	if err != nil || completedResumed.Status != core.TaskSucceeded || completedResumed.Attempt != 1 {
		t.Fatalf("resumed task completion = %+v, %v", completedResumed, err)
	}
	connection = resumedConnection
	installTask, err := dataStore.CreateTask(ctx, core.TaskRequest{
		AgentID: enrolled.AgentID, Action: core.ActionInstall, Engine: core.EngineMihomo, CoreVersion: core.CoreVersionDevelopment,
	})
	if err != nil {
		t.Fatalf("create core install task: %v", err)
	}
	taskMessage = core.WireMessage{}
	if err := wsjson.Read(ctx, connection, &taskMessage); err != nil {
		t.Fatalf("read core install task: %v", err)
	}
	if taskMessage.Task == nil || taskMessage.Task.ID != installTask.ID || taskMessage.Task.CoreVersion != core.CoreVersionDevelopment || taskMessage.Task.ConfigContent != "" {
		t.Fatalf("invalid core install task message: %+v", taskMessage)
	}
	result = core.WireMessage{Type: core.WireResult, Result: &core.TaskResultEnvelope{
		TaskID: installTask.ID,
		Result: core.TaskResultRequest{LeaseID: taskMessage.Task.LeaseID, Success: true, Output: "installed"},
	}}
	if err := wsjson.Write(ctx, connection, result); err != nil {
		t.Fatalf("write core install result: %v", err)
	}
	acknowledgment = core.WireMessage{}
	if err := wsjson.Read(ctx, connection, &acknowledgment); err != nil || acknowledgment.TaskID != installTask.ID {
		t.Fatalf("read core install acknowledgment: message=%+v error=%v", acknowledgment, err)
	}
	tasks, err := dataStore.ListTasks(ctx, enrolled.AgentID, 10)
	if err != nil || len(tasks) < 2 || tasks[0].Status != core.TaskSucceeded || tasks[0].CoreVersion != core.CoreVersionDevelopment {
		t.Fatalf("completed task not persisted: tasks=%+v error=%v", tasks, err)
	}
	for _, action := range []core.Action{core.ActionDeploy, core.ActionImportExisting, core.ActionReadConfig, core.ActionStart, core.ActionStop, core.ActionRestart} {
		request := core.TaskRequest{AgentID: enrolled.AgentID, Action: action, Engine: core.EngineMihomo}
		if action == core.ActionDeploy || action == core.ActionImportExisting {
			request.ConfigID = config.ID
		}
		created, createErr := dataStore.CreateTask(ctx, request)
		if createErr != nil {
			t.Fatalf("create %s task: %v", action, createErr)
		}
		var dispatched core.WireMessage
		if readErr := wsjson.Read(ctx, connection, &dispatched); readErr != nil || dispatched.Task == nil || dispatched.Task.ID != created.ID || dispatched.Task.Action != action {
			t.Fatalf("read %s task: message=%+v error=%v", action, dispatched, readErr)
		}
		output := "completed " + string(action)
		if action == core.ActionReadConfig {
			output = config.Content
		}
		completed := core.WireMessage{Type: core.WireResult, Result: &core.TaskResultEnvelope{
			TaskID: created.ID,
			Result: core.TaskResultRequest{LeaseID: dispatched.Task.LeaseID, Success: true, Output: output},
		}}
		if writeErr := wsjson.Write(ctx, connection, completed); writeErr != nil {
			t.Fatalf("write %s result: %v", action, writeErr)
		}
		var ack core.WireMessage
		if readErr := wsjson.Read(ctx, connection, &ack); readErr != nil || ack.TaskID != created.ID {
			t.Fatalf("read %s acknowledgment: message=%+v error=%v", action, ack, readErr)
		}
		stored, getErr := dataStore.GetTask(ctx, created.ID)
		if getErr != nil || stored.Status != core.TaskSucceeded {
			t.Fatalf("stored %s task = %+v, %v", action, stored, getErr)
		}
		if action == core.ActionReadConfig {
			snapshot, snapshotErr := dataStore.ReadTaskConfigSnapshot(ctx, created.ID, enrolled.AgentID, core.EngineMihomo)
			if snapshotErr != nil || snapshot != config.Content {
				t.Fatalf("read-config snapshot = %q, %v", snapshot, snapshotErr)
			}
			snapshotRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/tasks/"+created.ID+"/config-snapshot", nil)
			if requestErr != nil {
				t.Fatalf("create read-config snapshot request: %v", requestErr)
			}
			snapshotRequest.Header.Set("Authorization", "Bearer "+adminToken)
			snapshotResponse, requestErr := http.DefaultClient.Do(snapshotRequest)
			if requestErr != nil {
				t.Fatalf("read-config snapshot request: %v", requestErr)
			}
			var snapshotPayload struct {
				Content string `json:"content"`
			}
			decodeErr := json.NewDecoder(snapshotResponse.Body).Decode(&snapshotPayload)
			_ = snapshotResponse.Body.Close()
			if snapshotResponse.StatusCode != http.StatusOK || decodeErr != nil || snapshotPayload.Content != config.Content {
				t.Fatalf("read-config snapshot API status=%d content=%q decode=%v", snapshotResponse.StatusCode, snapshotPayload.Content, decodeErr)
			}
		}
	}
	deployments, err := dataStore.LatestDeployments(ctx)
	if err != nil {
		t.Fatalf("list deployments after task matrix: %v", err)
	}
	foundDeployment := false
	for _, deployment := range deployments {
		if deployment.AgentID == enrolled.AgentID {
			foundDeployment = true
			break
		}
	}
	if !foundDeployment {
		t.Fatal("successful deployment task did not update the latest deployment")
	}
	agent, err := dataStore.GetAgent(ctx, enrolled.AgentID)
	if err != nil || agent.Metrics.CollectedAt.IsZero() || agent.Metrics.CPUPercent != 42.5 || agent.Metrics.NetworkRXBPS != 300 || agent.Metrics.ObservedPublicIP != "93.184.216.34" || agent.Version != "test" || len(agent.Features) == 0 {
		t.Fatalf("live metrics snapshot not persisted: agent=%+v error=%v", agent, err)
	}
	if err := dataStore.SetAgentClientAddress(ctx, enrolled.AgentID, "managed.example.test"); err != nil {
		t.Fatalf("set managed client address: %v", err)
	}
	managedAgent, err := dataStore.GetAgent(ctx, enrolled.AgentID)
	if candidates := clientAddressCandidates(managedAgent); err != nil || len(candidates) == 0 || candidates[0].address != "managed.example.test" || candidates[0].source != "手动设置" {
		t.Fatalf("managed client address did not take priority: candidates=%+v error=%v", candidates, err)
	}
	if err := dataStore.SetAgentClientAddress(ctx, enrolled.AgentID, ""); err != nil {
		t.Fatalf("restore automatic client address: %v", err)
	}
	automaticAgent, err := dataStore.GetAgent(ctx, enrolled.AgentID)
	if candidates := clientAddressCandidates(automaticAgent); err != nil || len(candidates) == 0 || candidates[0].address != "93.184.216.34" || candidates[0].source != "控制面实时观测公网地址" {
		t.Fatalf("automatic client address did not resume the WSS observation: candidates=%+v error=%v", candidates, err)
	}
	if err := dataStore.UpdateAgentObservedPublicIP(ctx, enrolled.AgentID, ""); err != nil {
		t.Fatalf("clear unavailable WSS public observation: %v", err)
	}
	fallbackAgent, err := dataStore.GetAgent(ctx, enrolled.AgentID)
	if candidates := clientAddressCandidates(fallbackAgent); err != nil || len(candidates) == 0 || candidates[0].address != "192.0.2.20" || candidates[0].source != "Agent 默认路由接口 eth0" {
		t.Fatalf("unavailable public source did not retain the default-route fallback: candidates=%+v error=%v", candidates, err)
	}
	revokedTask, err := dataStore.CreateTask(ctx, core.TaskRequest{
		AgentID: enrolled.AgentID, Action: core.ActionStatus, Engine: core.EngineMihomo,
	})
	if err != nil {
		t.Fatalf("create task before agent revocation: %v", err)
	}

	deleteRequest, _ := http.NewRequestWithContext(ctx, http.MethodDelete, httpServer.URL+"/api/v1/agents/"+enrolled.AgentID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer "+adminToken)
	deleteResponse, err := http.DefaultClient.Do(deleteRequest)
	if err != nil {
		t.Fatalf("delete connected agent: %v", err)
	}
	deleteResponse.Body.Close()
	if deleteResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("delete connected agent status = %s", deleteResponse.Status)
	}
	disconnectContext, cancelDisconnect := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDisconnect()
	for {
		var afterRevocation core.WireMessage
		if err := wsjson.Read(disconnectContext, connection, &afterRevocation); err != nil {
			break
		}
	}
	if errors.Is(disconnectContext.Err(), context.DeadlineExceeded) {
		t.Fatal("revoked Agent WSS was not closed promptly")
	}
	if _, err := dataStore.GetAgent(ctx, enrolled.AgentID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked agent remains queryable: %v", err)
	}
	terminatedTask, err := dataStore.GetTask(ctx, revokedTask.ID)
	if err != nil || terminatedTask.Status != core.TaskFailed || terminatedTask.Error != "agent identity was revoked" || terminatedTask.FinishedAt == nil {
		t.Fatalf("task after agent revocation = %+v, %v", terminatedTask, err)
	}
	if _, err := dataStore.AgentConfig(ctx, enrolled.AgentID, core.EngineMihomo); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked agent configuration remains active: %v", err)
	}
	if _, err := dataStore.ConfigRevision(ctx, config.ID, config.Version); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked agent configuration history remains available: %v", err)
	}

	rejectedHandshake, _ := http.NewRequestWithContext(ctx, http.MethodGet, websocketURL, nil)
	if err := authn.SignRequest(rejectedHandshake, nil, enrolled.AgentID, privateKey, time.Now().UTC()); err != nil {
		t.Fatalf("sign revoked WSS handshake: %v", err)
	}
	rejectedConnection, rejectedResponse, err := websocket.Dial(ctx, websocketURL, &websocket.DialOptions{
		HTTPHeader: rejectedHandshake.Header, Subprotocols: []string{"qcontrolhub.agent.v1"},
	})
	if rejectedConnection != nil {
		rejectedConnection.CloseNow()
	}
	if err == nil || rejectedResponse == nil || rejectedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked Agent reconnect = connection=%v response=%v error=%v", rejectedConnection, rejectedResponse, err)
	}
	rejectedResponse.Body.Close()
}
