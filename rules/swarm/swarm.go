// Copyright 2016 The Prometheus Authors
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
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/mwitkow/go-conntrack"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/util/yaml"
	"regexp"
)

const (
	// metaLabelPrefix is the meta prefix used for all meta labels in this discovery.
	metaLabelPrefix = model.MetaLabelPrefix + "swarm_"
	// serviceLabelPrefix is the prefix for the service labels.
	serviceLabelPrefix = metaLabelPrefix + "service_label_"

	// serviceLabel is used for the name of the service in Swarm.
	serviceLabel model.LabelName = metaLabelPrefix + "service"
	// imageLabel is the label that is used for the docker image running the service.
	imageLabel model.LabelName = metaLabelPrefix + "image"
	// taskLabel contains the task name of the app instance.
	taskLabel model.LabelName = metaLabelPrefix + "task"
	// nodeIPLabel contains the node ip.
	nodeIPLabel model.LabelName = metaLabelPrefix + "node_ip"
	// nodeNameLabel contains the node name.
	nodeNameLabel model.LabelName = metaLabelPrefix + "node_name"

	// Constants for instrumentation.
	namespace = "prometheus"

	// apiVersion is the default version of Docker engine API.
	apiVersion = "1.32"
	// labelPrefix is the prefix of service labels used for filtering in this discovery.
	labelPrefix = "prometheus."
)

var (

	// RuleRegexp used to extract the rule label of the service
	RuleRegexp = regexp.MustCompile(`^prometheus\.rules\.(?P<rule_name>.+?)$`)

	refreshFailuresCount = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rd_swarm_refresh_failures_total",
			Help:      "The number of Swarm-SD refresh failures.",
		})
	refreshDuration = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Namespace: namespace,
			Name:      "rd_swarm_refresh_duration_seconds",
			Help:      "The duration of a Swarm-SD refresh in seconds.",
		})
	// DefaultSDConfig is the default Swarm SD configuration.
	DefaultSDConfig = SDConfig{
		Timeout:         model.Duration(30 * time.Second),
		RefreshInterval: model.Duration(30 * time.Second),
	}
)

// SDConfig is the configuration for services running on Swarm.
type SDConfig struct {
	APIServer       string         `yaml:"api_server"`
	APIVersion      string         `yaml:"api_version"`
	Group           string         `yaml:"group,omitempty"`
	Network         string         `yaml:"network,omitempty"`
	Timeout         model.Duration `yaml:"timeout,omitempty"`
	RefreshInterval model.Duration `yaml:"refresh_interval,omitempty"`

	// Catches all undefined fields and must be empty after parsing.
	XXX map[string]interface{} `yaml:",inline"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	if err := yaml.CheckOverflow(c.XXX, "swarm_sd_config"); err != nil {
		return err
	}
	if c.APIServer == "" {
		return fmt.Errorf("'api_server' must be configured")
	}

	return nil
}

func init() {
	prometheus.MustRegister(refreshFailuresCount)
	prometheus.MustRegister(refreshDuration)
}

// Discovery provides service discovery based on a Swarm instance.
type Discovery struct {
	client          *http.Client
	apiServer       string
	apiVersion      string
	group           string
	network         string
	refreshInterval time.Duration
	logger          log.Logger
}

// NewDiscovery returns a new Swarm Discovery.
func NewDiscovery(conf SDConfig, logger log.Logger) (*Discovery, error) {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	client := &http.Client{
		Timeout: time.Duration(conf.Timeout),
		Transport: &http.Transport{
			DialContext: conntrack.NewDialContextFunc(
				conntrack.DialWithTracing(),
				conntrack.DialWithName("swarm_sd"),
			),
		},
	}

	d := &Discovery{
		client:          client,
		apiServer:       strings.TrimRight(conf.APIServer, "/"),
		group:           conf.Group,
		network:         conf.Network,
		refreshInterval: time.Duration(conf.RefreshInterval),
		logger:          logger,
	}
	if conf.APIVersion == "" {
		d.apiVersion = apiVersion
	} else {
		d.apiVersion = conf.APIVersion
	}
	return d, nil
}

func (d *Discovery) File() string {
	return d.apiServer + "/" + d.network + "/" + d.apiVersion
}

// Run implements the TargetProvider interface.
func (d *Discovery) Run(ctx context.Context, ch chan<- []*RuleGroupConfig) {
	err := d.updateServices(ctx, ch)
	if err != nil {
		level.Error(d.logger).Log("msg", "Error while updating services", "err", err)
	}

	t := time.NewTicker(d.refreshInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err = d.updateServices(ctx, ch)
			if err != nil {
				level.Error(d.logger).Log("msg", "Error while updating services", "err", err)
			}
		}
	}
}

func (d *Discovery) updateServices(ctx context.Context, ch chan<- []*RuleGroupConfig) (err error) {
	start := time.Now()
	defer func() {
		refreshDuration.Observe(time.Since(start).Seconds())
		if err != nil {
			refreshFailuresCount.Inc()
		}
	}()

	targetMap, err := d.fetchRuleGroups()
	if err != nil {
		return err
	}

	all := make([]*RuleGroupConfig, 0, len(targetMap))
	for _, tg := range targetMap {
		all = append(all, tg)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case ch <- all:
	}

	return nil
}

func (d *Discovery) fetchRuleGroups() (map[string]*RuleGroupConfig, error) {
	services, err := d.fetchServices()
	if err != nil {
		return nil, err
	}

	nodes, err := d.fetchNodes()
	if err != nil {
		return nil, err
	}

	groups := map[string]*RuleGroupConfig{}
	for _, service := range services {
		if len(service.Tasks) > 0 {
			group := d.createRuleGroup(service, nodes)
			if len(group.Rules) > 0 {
				groups[group.Name] = group
			}
		}
	}
	return groups, nil
}

func (d *Discovery) createRuleGroup(service *Service, nodes map[string]*Node) *RuleGroupConfig {

	ruleLabels := make(map[string]string)

	for name, value := range service.Spec.Labels {
		ruleLabel := RuleRegexp.FindStringSubmatch(name)
		if len(ruleLabel) > 1 {
			ruleLabels[ruleLabel[1]] = value
		}
	}

	ruleList := make([]RuleConfig, 0, len(ruleLabels))
	for _, ruleJSON := range ruleLabels {
		r := RuleConfig{}
		err := json.Unmarshal([]byte(ruleJSON), &r)
		if err != nil {
			continue
		}

		ruleList = append(ruleList, r)
	}

	g := &RuleGroupConfig {
		Name: service.Spec.Name,
		Rules: ruleList,
	}

	return g
}

// fetchServices requests a list of services from Swarm cluster.
func (d *Discovery) fetchServices() ([]*Service, error) {
	filters := Args{}
	filters.Add("label", labelPrefix+"enable=true")
	body, err := d.request("/services", filters)
	if err != nil {
		return nil, err
	}

	services := make([]*Service, 0)
	err = json.Unmarshal(body, &services)
	if err != nil {
		return nil, err
	}

	for _, service := range services {
		err = d.fetchTasks(service)
		if err != nil {
			return nil, err
		}
	}
	return services, nil
}

// fetchTasks requests a list of tasks for service.
func (d *Discovery) fetchTasks(service *Service) error {
	filters := Args{}
	filters.Add("service", service.Spec.Name)
	filters.Add("desired-state", "running")
	body, err := d.request("/tasks", filters)
	if err != nil {
		return err
	}

	return json.Unmarshal(body, &service.Tasks)
}

// fetchNodes requests a list of nodes.
func (d *Discovery) fetchNodes() (map[string]*Node, error) {
	filters := Args{}
	body, err := d.request("/nodes", filters)
	if err != nil {
		return nil, err
	}

	nodes := make([]*Node, 0)
	err = json.Unmarshal(body, &nodes)
	if err != nil {
		return nil, err
	}

	m := make(map[string]*Node)
	for _, node := range nodes {
		m[node.ID] = node
	}
	return m, nil
}

// fetchServices requests a list of services from Swarm cluster.
func (d *Discovery) request(path string, args Args) ([]byte, error) {
	filters, err := args.ToJSON()
	if err != nil {
		return nil, err
	}

	query := url.Values{}
	if filters != "" {
		query.Set("filters", filters)
	}
	u := fmt.Sprintf("%s/v%s%s?%s", d.apiServer, d.apiVersion, path, query.Encode())
	request, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := d.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (d *Discovery) targetsForService(service *Service, nodes map[string]*Node) []model.LabelSet {
	targets := make([]model.LabelSet, 0, len(service.Tasks))
	network := service.Spec.Labels[labelPrefix+"network"]
	if network == "" {
		network = d.network
	}
	port := service.Spec.Labels[labelPrefix+"port"]
	path := service.Spec.Labels[labelPrefix+"path"]
	group := service.Spec.Labels[labelPrefix+"group"]
	//scheme := service.Spec.Labels[labelPrefix+"scheme"]
	if port != "" && group == d.group {
		for _, t := range service.Tasks {
			node, ok := nodes[t.NodeID]
			if !ok {
				continue
			}

			addr := d.getTaskAddr(&t, network, node)
			if addr != "" {
				target := model.LabelSet{
					model.AddressLabel: model.LabelValue(net.JoinHostPort(addr, port)),
					taskLabel:          model.LabelValue(t.ID),
					nodeIPLabel:        model.LabelValue(d.getNodeIP(node)),
					nodeNameLabel:      model.LabelValue(d.getNodeName(node)),
				}
				if path != "" {
					target[model.MetricsPathLabel] = model.LabelValue(path)
				}
				targets = append(targets, target)
			}
		}
	}
	return targets
}

func (d *Discovery) getTaskAddr(t *Task, network string, node *Node) string {
	if network == "host" {
		return d.getNodeIP(node)
	} else if len(t.NetworksAttachments) > 0 {
		var addrs []string
		for _, n := range t.NetworksAttachments {
			if n.Network.Spec.Name == network {
				addrs = n.Addresses
				break
			}
		}
		if addrs == nil {
			// use first network as default
			addrs = t.NetworksAttachments[0].Addresses
		}
		if len(addrs) > 0 {
			return strings.Split(addrs[0], "/")[0]
		}
	}
	return ""
}

func (d *Discovery) getNodeIP(n *Node) string {
	if n.Status.Addr != "0.0.0.0" {
		return n.Status.Addr
	}
	host, _, _ := net.SplitHostPort(n.ManagerStatus.Addr)
	return host
}

func (d *Discovery) getNodeName(n *Node) string {
	if n.Spec.Name != "" {
		return n.Spec.Name
	}
	return n.Description.Hostname
}

// RuleGroup is a list of sequentially evaluated recording and alerting rules.
type RuleGroupConfig struct {
	Name     string         `json:"name"`
	Interval model.Duration `json:"interval,omitempty"`
	Rules    []RuleConfig         `json:"rules"`
}

// Rule describes an alerting or recording rule.
type RuleConfig struct {
	Record      string            `json:"record,omitempty"`
	Alert       string            `json:"alert,omitempty"`
	Expr        string            `json:"expr"`
	For         string    `json:"for,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Service describes one instance of a service running on Swarm.
type Service struct {
	Spec struct {
		Name         string
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct {
				Image string
			}
		}
	}
	Tasks []Task
}

// Task describes one instance of a service running on Swarm.
type Task struct {
	ID     string
	NodeID string
	Spec   struct {
		ContainerSpec struct {
			Image string
		}
	}
	NetworksAttachments []struct {
		Network struct {
			Spec struct {
				Name string
			}
		}
		Addresses []string
	}
}

// Node is an instance of the Engine participating in a swarm.
type Node struct {
	ID   string
	Spec struct {
		Name string
	}
	Status struct {
		Addr string
	}
	ManagerStatus struct {
		Addr string
	}
	Description struct {
		Hostname string
	}
}

// Args stores a mapping of keys to a set of multiple values.
type Args struct {
	fields map[string]map[string]bool
}

// Add a new value to the set of values
func (args *Args) Add(key, value string) {
	if args.fields == nil {
		args.fields = make(map[string]map[string]bool)
	}

	if _, ok := args.fields[key]; ok {
		args.fields[key][value] = true
	} else {
		args.fields[key] = map[string]bool{value: true}
	}
}

// ToJSON returns the Args as a JSON encoded string
func (args *Args) ToJSON() (string, error) {
	if len(args.fields) == 0 {
		return "", nil
	}
	buf, err := json.Marshal(args.fields)
	return string(buf), err
}
