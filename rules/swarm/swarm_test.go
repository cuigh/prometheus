// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/prometheus/util/testutil"
	"strconv"
)

var (
	testService = "prometheus"
	alertingRule = &RuleConfig{
		Alert: "TestAlert",
		Expr: "testAlertExpr",
		For: "15s",
		Labels:  map[string]string { "traefik.network": "traefik-front" },
		Annotations:  map[string]string { "description": "Test Alert" },
	}
	recordRule = &RuleConfig{
		Record: "TestRecord",
		Expr: "testRecordExpr",
		Labels:  map[string]string { "traefik.network": "traefik-front" },
		Annotations:  map[string]string { "description": "Test record" },
	}
)

type testSwarmServer struct {
	err        string
	apiVersion string
	rule  *RuleConfig
	*httptest.Server
}

func newTestServer(apiVersion string, err string, rule *RuleConfig) *testSwarmServer {
	const (
		nodeID      = "1"
		networkName = "monitor"
		image       = "prom/prometheus"
	)

	s := &testSwarmServer{
		err:        err,
		apiVersion: apiVersion,
		rule:  rule,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/v%s/nodes", s.apiVersion), func(w http.ResponseWriter, r *http.Request) {
		if s.err != "" {
			http.Error(w, s.err, http.StatusInternalServerError)
			return
		}

		n := Node{ID: nodeID}
		n.Spec.Name = "docker-01"
		n.Status.Addr = "192.168.1.100"
		//n.Status.Addr = "0.0.0.0"
		n.ManagerStatus.Addr = "192.168.1.100:2377"
		n.Description.Hostname = "docker-01"
		json.NewEncoder(w).Encode([]Node{n})
	})
	mux.HandleFunc(fmt.Sprintf("/v%s/services", s.apiVersion), func(w http.ResponseWriter, r *http.Request) {
		if s.err != "" {
			http.Error(w, s.err, http.StatusInternalServerError)
			return
		}

		service := Service{}
		service.Spec.Name = "prometheus"
		service.Spec.Labels = map[string]string{
			labelPrefix + "enable":  "true",
			labelPrefix + "port":    "9090",
			labelPrefix + "network": networkName,
		}
		if s.rule != nil {
			b, err := json.Marshal(s.rule)
			if err != nil {
				http.Error(w, fmt.Sprintf("json.Marshal error: %s ", err), http.StatusInternalServerError)
				return
			}
			service.Spec.Labels[labelPrefix + "rules.TestAlert"] = string(b)
		}
		service.Spec.TaskTemplate.ContainerSpec.Image = image
		json.NewEncoder(w).Encode([]Service{service})
	})
	mux.HandleFunc(fmt.Sprintf("/v%s/tasks", s.apiVersion), func(w http.ResponseWriter, r *http.Request) {
		if s.err != "" {
			http.Error(w, s.err, http.StatusInternalServerError)
			return
		}

		var tasks []Task
		for i := 0; i < 2; i++ {
			t := Task{
				ID:     strconv.Itoa(i + 1),
				NodeID: nodeID,
			}
			t.Spec.ContainerSpec.Image = image
			t.NetworksAttachments = append(t.NetworksAttachments, struct {
				Network   struct{ Spec struct{ Name string } }
				Addresses []string
			}{
				Network: struct{ Spec struct{ Name string } }{
					Spec: struct{ Name string }{Name: networkName},
				},
				Addresses: []string{fmt.Sprintf("10.0.2.1%v/24", i)}},
			)
			tasks = append(tasks, t)
		}
		json.NewEncoder(w).Encode(tasks)
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func testUpdateServices(s *testSwarmServer, ch chan []*RuleGroupConfig) error {
	conf := SDConfig{APIServer: s.URL}
	md, err := NewDiscovery(conf, nil)
	if err != nil {
		return err
	}
	return md.updateServices(context.Background(), ch)
}

func TestSwarmSDHandleError(t *testing.T) {
	const testError = "testing failure"

	ch := make(chan []*RuleGroupConfig, 1)
	s := newTestServer(apiVersion, testError, alertingRule)
	defer s.Close()

	err := testUpdateServices(s, ch)
	testutil.Assert(t, err.Error() == testError, "Expected error: %s, got none", testError)

	select {
	case tg := <-ch:
		t.Fatalf("Got group: %s", tg)
	default:
	}
}

func TestSwarmSDEmptyList(t *testing.T) {
	ch := make(chan []*RuleGroupConfig, 1)
	s := newTestServer(apiVersion, "", nil)
	defer s.Close()

	err := testUpdateServices(s, ch)
	testutil.Ok(t, err)

	select {
	case tg := <-ch:
		if len(tg) > 0 {
			t.Fatalf("Got group: %v", tg)
		}
	default:
	}
}

func TestSwarmSDSendGroup(t *testing.T) {
	ch := make(chan []*RuleGroupConfig, 1)
	s := newTestServer(apiVersion, "", alertingRule)
	defer s.Close()

	err := testUpdateServices(s, ch)
	testutil.Ok(t, err)

	select {
	case rgs := <-ch:
		rg := rgs[0]
		testutil.Assert(t, rg.Name == testService, "Wrong rule group name: %s", rg.Name)

		rule := rg.Rules[0]
		testutil.Assert(t, rule.Alert == alertingRule.Alert, "Wrong alert name: %s", rule.Alert)
	default:
		t.Fatal("Did not get a rule group.")
	}
}

func TestSwarmSDChangeRule(t *testing.T) {
	ch := make(chan []*RuleGroupConfig, 1)
	s := newTestServer(apiVersion, "", alertingRule)
	defer s.Close()

	err := testUpdateServices(s, ch)
	testutil.Ok(t, err)
	up1 := (<-ch)[0]
	testutil.Assert(t, len(up1.Rules) == 1, "Expected %v targets, got %v", 1, len(up1.Rules))

	s.rule = recordRule
	err = testUpdateServices(s, ch)
	testutil.Ok(t, err)
	up2 := (<-ch)[0]
	testutil.Assert(t, len(up2.Rules) == 1, "Expected %v targets, got %v", 1, len(up2.Rules))

	testutil.Assert(t, up1.Name == up2.Name, "Name is different: %s", up2)

	testutil.Assert(t, up1.Rules[0].Expr != up2.Rules[0].Expr, "Expr is not different: %s", up2)
}
