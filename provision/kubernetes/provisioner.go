// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package kubernetes

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/image"
	tsuruErrors "github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/log"
	tsuruNet "github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/dockercommon"
	"github.com/tsuru/tsuru/provision/servicecommon"
	"github.com/tsuru/tsuru/set"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	policy "k8s.io/client-go/pkg/apis/policy/v1beta1"
	"k8s.io/client-go/pkg/labels"
	"k8s.io/kubernetes/pkg/util/term"
)

const (
	provisionerName        = "kubernetes"
	tsuruNamespace         = "default"
	dockerImageName        = "docker:1.11.2"
	defaultBuildJobTimeout = 30 * time.Minute
)

type kubernetesProvisioner struct{}

var (
	_ provision.Provisioner              = &kubernetesProvisioner{}
	_ provision.UploadDeployer           = &kubernetesProvisioner{}
	_ provision.ShellProvisioner         = &kubernetesProvisioner{}
	_ provision.NodeProvisioner          = &kubernetesProvisioner{}
	_ provision.NodeContainerProvisioner = &kubernetesProvisioner{}
	_ provision.ExecutableProvisioner    = &kubernetesProvisioner{}
	// _ provision.ArchiveDeployer          = &kubernetesProvisioner{}
	// _ provision.ImageDeployer            = &kubernetesProvisioner{}
	// _ provision.MessageProvisioner       = &kubernetesProvisioner{}
	// _ provision.InitializableProvisioner = &kubernetesProvisioner{}
	// _ provision.RollbackableDeployer     = &kubernetesProvisioner{}
	// _ provision.RebuildableDeployer      = &kubernetesProvisioner{}
	// _ provision.MetricsProvisioner       = &kubernetesProvisioner{}
	// _ provision.SleepableProvisioner     = &kubernetesProvisioner{}
	// _ provision.OptionalLogsProvisioner  = &kubernetesProvisioner{}
	// _ provision.UnitStatusProvisioner    = &kubernetesProvisioner{}
	// _ provision.NodeRebalanceProvisioner = &kubernetesProvisioner{}
	// _ provision.AppFilterProvisioner     = &kubernetesProvisioner{}
	// _ provision.ExtensibleProvisioner    = &kubernetesProvisioner{}
)

func init() {
	provision.Register(provisionerName, func() (provision.Provisioner, error) {
		return &kubernetesProvisioner{}, nil
	})
}

func (p *kubernetesProvisioner) GetName() string {
	return provisionerName
}

func (p *kubernetesProvisioner) Provision(provision.App) error {
	return nil
}

func (p *kubernetesProvisioner) Destroy(a provision.App) error {
	imgID, err := image.AppCurrentImageName(a.GetName())
	if err != nil {
		return errors.WithStack(err)
	}
	data, err := image.GetImageCustomData(imgID)
	if err != nil {
		return errors.WithStack(err)
	}
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	manager := &serviceManager{
		client: client,
	}
	multiErrors := tsuruErrors.NewMultiError()
	for process := range data.Processes {
		err = manager.RemoveService(a, process)
		if err != nil {
			multiErrors.Add(err)
		}
	}
	if multiErrors.Len() > 0 {
		return multiErrors
	}
	return nil
}

func (p *kubernetesProvisioner) AddUnits(a provision.App, units uint, processName string, w io.Writer) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	return servicecommon.ChangeUnits(&serviceManager{
		client: client,
	}, a, int(units), processName)
}

func (p *kubernetesProvisioner) RemoveUnits(a provision.App, units uint, processName string, w io.Writer) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	return servicecommon.ChangeUnits(&serviceManager{
		client: client,
	}, a, -int(units), processName)
}

func (p *kubernetesProvisioner) Restart(a provision.App, process string, w io.Writer) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Start: true, Restart: true})
}

func (p *kubernetesProvisioner) Start(a provision.App, process string) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Start: true})
}

func (p *kubernetesProvisioner) Stop(a provision.App, process string) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Stop: true})
}

var stateMap = map[v1.PodPhase]provision.Status{
	v1.PodPending:   provision.StatusCreated,
	v1.PodRunning:   provision.StatusStarted,
	v1.PodSucceeded: provision.StatusStopped,
	v1.PodFailed:    provision.StatusError,
	v1.PodUnknown:   provision.StatusError,
}

func (p *kubernetesProvisioner) podsToUnits(client kubernetes.Interface, pods []v1.Pod, baseApp provision.App, baseNode *v1.Node) ([]provision.Unit, error) {
	var err error
	if len(pods) == 0 {
		return nil, nil
	}
	nodeMap := map[string]*v1.Node{}
	appMap := map[string]provision.App{}
	webProcMap := map[string]string{}
	portMap := map[string]int32{}
	if baseApp != nil {
		appMap[baseApp.GetName()] = baseApp
	}
	if baseNode != nil {
		nodeMap[baseNode.Name] = baseNode
	}
	units := make([]provision.Unit, len(pods))
	for i, pod := range pods {
		l := labelSetFromMeta(&pod.ObjectMeta)
		node, ok := nodeMap[pod.Spec.NodeName]
		if !ok {
			node, err = client.Core().Nodes().Get(pod.Spec.NodeName)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			nodeMap[pod.Spec.NodeName] = node
		}
		podApp, ok := appMap[l.AppName()]
		if !ok {
			podApp, err = app.GetByName(l.AppName())
			if err != nil {
				return nil, errors.WithStack(err)
			}
			appMap[podApp.GetName()] = podApp
		}
		webProcessName, ok := webProcMap[podApp.GetName()]
		if !ok {
			var imageName string
			imageName, err = image.AppCurrentImageName(podApp.GetName())
			if err != nil {
				return nil, errors.WithStack(err)
			}
			webProcessName, err = image.GetImageWebProcessName(imageName)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			webProcMap[podApp.GetName()] = webProcessName
		}
		wrapper := kubernetesNodeWrapper{node: node, prov: p}
		url := &url.URL{
			Scheme: "http",
			Host:   wrapper.Address(),
		}
		appProcess := l.AppProcess()
		if appProcess != "" && appProcess == webProcessName {
			srvName := deploymentNameForApp(podApp, webProcessName)
			port, ok := portMap[srvName]
			if !ok {
				port, err = getServicePort(client, srvName)
				if err != nil {
					return nil, err
				}
				portMap[srvName] = port
			}
			url.Host = fmt.Sprintf("%s:%d", url.Host, port)
		}
		units[i] = provision.Unit{
			ID:          pod.Name,
			Name:        pod.Name,
			AppName:     l.AppName(),
			ProcessName: appProcess,
			Type:        l.AppPlatform(),
			Ip:          tsuruNet.URLToHost(wrapper.Address()),
			Status:      stateMap[pod.Status.Phase],
			Address:     url,
		}
	}
	return units, nil
}

func (p *kubernetesProvisioner) Units(a provision.App) ([]provision.Unit, error) {
	client, err := getClusterClient()
	if err != nil {
		return nil, err
	}
	l, err := provision.ServiceLabels(provision.ServiceLabelsOpts{App: a, Provisioner: provisionerName, Prefix: tsuruLabelPrefix})
	if err != nil {
		return nil, err
	}
	pods, err := client.Core().Pods(tsuruNamespace).List(v1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set(l.ToAppSelector())).String(),
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return p.podsToUnits(client, pods.Items, a, nil)
}

func (p *kubernetesProvisioner) RoutableAddresses(a provision.App) ([]url.URL, error) {
	client, err := getClusterClient()
	if err != nil {
		return nil, err
	}
	imageName, err := image.AppCurrentImageName(a.GetName())
	if err != nil {
		if err != image.ErrNoImagesAvailable {
			return nil, err
		}
		return nil, nil
	}
	webProcessName, err := image.GetImageWebProcessName(imageName)
	if err != nil {
		return nil, err
	}
	if webProcessName == "" {
		return nil, nil
	}
	srvName := deploymentNameForApp(a, webProcessName)
	pubPort, err := getServicePort(client, srvName)
	if err != nil {
		return nil, err
	}
	nodes, err := client.Core().Nodes().List(v1.ListOptions{
		LabelSelector: fmt.Sprintf("pool=%s", a.GetPool()),
	})
	if err != nil {
		return nil, err
	}
	addrs := make([]url.URL, len(nodes.Items))
	for i, n := range nodes.Items {
		wrapper := kubernetesNodeWrapper{node: &n, prov: p}
		addrs[i] = url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", wrapper.Address(), pubPort),
		}
	}
	return addrs, nil
}

func (p *kubernetesProvisioner) RegisterUnit(a provision.App, unitID string, customData map[string]interface{}) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	pod, err := client.Core().Pods(tsuruNamespace).Get(unitID)
	if err != nil {
		return errors.WithStack(err)
	}
	units, err := p.podsToUnits(client, []v1.Pod{*pod}, a, nil)
	if err != nil {
		return err
	}
	if len(units) == 0 {
		return errors.Errorf("unable to convert pod to unit: %#v", pod)
	}
	err = a.BindUnit(&units[0])
	if err != nil {
		return errors.WithStack(err)
	}
	if customData == nil {
		return nil
	}
	l := labelSetFromMeta(&pod.ObjectMeta)
	buildingImage := l.BuildImage()
	if buildingImage == "" {
		return nil
	}
	return errors.WithStack(image.SaveImageCustomData(buildingImage, customData))
}

func (p *kubernetesProvisioner) ListNodes(addressFilter []string) ([]provision.Node, error) {
	client, cfg, err := getClusterClientWithCfg()
	if err != nil {
		if err == errNoCluster {
			return nil, nil
		}
		return nil, err
	}
	var nodes []provision.Node
	var addressSet set.Set
	if len(addressFilter) > 0 {
		addressSet = set.FromSlice(addressFilter)
	}
	if addressSet == nil || addressSet.Includes(cfg.Host) {
		nodes = append(nodes, &clusterNode{address: cfg.Host, prov: p})
	}
	nodeList, err := client.Core().Nodes().List(v1.ListOptions{})
	if err != nil {
		// TODO(cezarsa): It would be better to return an error to be handled
		// by the api. Failing to list nodes from one provisioner should not
		// prevent other nodes from showing up.
		log.Errorf("unable to list all node from kubernetes cluster: %v", err)
		return nodes, nil
	}
	for i := range nodeList.Items {
		n := &kubernetesNodeWrapper{
			node: &nodeList.Items[i],
			prov: p,
		}
		if addressSet == nil || addressSet.Includes(n.Address()) {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

func (p *kubernetesProvisioner) GetNode(address string) (provision.Node, error) {
	client, cfg, err := getClusterClientWithCfg()
	if err != nil {
		if err == errNoCluster {
			return nil, provision.ErrNodeNotFound
		}
		return nil, err
	}
	if address == cfg.Host {
		return &clusterNode{address: cfg.Host, prov: p}, nil
	}
	node, err := p.findNodeByAddress(client, address)
	if err != nil {
		return nil, err
	}
	return node, nil
}

func (p *kubernetesProvisioner) AddNode(opts provision.AddNodeOptions) error {
	isCluster, _ := strconv.ParseBool(opts.Metadata["cluster"])
	if isCluster {
		err := addClusterNode(opts)
		if err != nil {
			return err
		}
		client, err := getClusterClient()
		if err != nil {
			return err
		}
		m := nodeContainerManager{client: client}
		return servicecommon.EnsureNodeContainersCreated(&m, ioutil.Discard)
	}
	// TODO(cezarsa): Start kubelet, kube-proxy and add labels
	return errors.New("adding nodes to cluster not supported yet on kubernetes")
}

func (p *kubernetesProvisioner) RemoveNode(opts provision.RemoveNodeOptions) error {
	client, cfg, err := getClusterClientWithCfg()
	if err != nil {
		return err
	}
	if opts.Address == cfg.Host {
		return removeClusterNode(opts.Address)
	}
	nodeWrapper, err := p.findNodeByAddress(client, opts.Address)
	if err != nil {
		return err
	}
	node := nodeWrapper.node
	if opts.Rebalance {
		node.Spec.Unschedulable = true
		_, err = client.Core().Nodes().Update(node)
		if err != nil {
			return errors.WithStack(err)
		}
		var pods []v1.Pod
		pods, err = podsFromNode(client, node.Name)
		if err != nil {
			return err
		}
		for _, pod := range pods {
			err = client.Core().Pods(tsuruNamespace).Evict(&policy.Eviction{
				ObjectMeta: v1.ObjectMeta{
					Name:      pod.Name,
					Namespace: tsuruNamespace,
				},
			})
			if err != nil {
				return errors.WithStack(err)
			}
		}
	}
	err = client.Core().Nodes().Delete(node.Name, &v1.DeleteOptions{})
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (p *kubernetesProvisioner) NodeForNodeData(nodeData provision.NodeStatusData) (provision.Node, error) {
	return provision.FindNodeByAddrs(p, nodeData.Addrs)
}

func (p *kubernetesProvisioner) findNodeByAddress(client kubernetes.Interface, address string) (*kubernetesNodeWrapper, error) {
	nodeList, err := client.Core().Nodes().List(v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range nodeList.Items {
		nodeWrapper := &kubernetesNodeWrapper{node: &nodeList.Items[i], prov: p}
		if address == nodeWrapper.Address() {
			return nodeWrapper, nil
		}
	}
	return nil, provision.ErrNodeNotFound
}

func (p *kubernetesProvisioner) UpdateNode(opts provision.UpdateNodeOptions) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	nodeWrapper, err := p.findNodeByAddress(client, opts.Address)
	if err != nil {
		return err
	}
	node := nodeWrapper.node
	if opts.Disable {
		node.Spec.Unschedulable = true
	} else if opts.Enable {
		node.Spec.Unschedulable = false
	}
	for k, v := range opts.Metadata {
		if v == "" {
			delete(node.Labels, k)
		} else {
			node.Labels[k] = v
		}
	}
	_, err = client.Core().Nodes().Update(node)
	return err
}

func (p *kubernetesProvisioner) UploadDeploy(a provision.App, archiveFile io.ReadCloser, fileSize int64, build bool, evt *event.Event) (string, error) {
	defer archiveFile.Close()
	if build {
		return "", errors.New("running UploadDeploy with build=true is not yet supported")
	}
	deployJobName := deployJobNameForApp(a)
	baseImage := image.GetBuildImage(a)
	buildingImage, err := image.AppNewImageName(a.GetName())
	if err != nil {
		return "", errors.WithStack(err)
	}
	client, err := getClusterClient()
	if err != nil {
		return "", err
	}
	defer cleanupJob(client, deployJobName)
	cmds := dockercommon.ArchiveDeployCmds(a, "file:///home/application/archive.tar.gz")
	if len(cmds) != 3 {
		return "", errors.Errorf("unexpected cmds list: %#v", cmds)
	}
	cmds[2] = fmt.Sprintf("cat >/home/application/archive.tar.gz && %s", cmds[2])
	params := buildJobParams{
		app:              a,
		client:           client,
		buildCmd:         cmds,
		sourceImage:      baseImage,
		destinationImage: buildingImage,
		attachInput:      archiveFile,
		attachOutput:     evt,
	}
	_, err = createBuildJob(params)
	if err != nil {
		return "", err
	}
	err = waitForJob(client, deployJobName, defaultBuildJobTimeout)
	if err != nil {
		return "", err
	}
	manager := &serviceManager{
		client: client,
	}
	err = servicecommon.RunServicePipeline(manager, a, buildingImage, nil)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return buildingImage, nil
}

func (p *kubernetesProvisioner) UpgradeNodeContainer(name string, pool string, writer io.Writer) error {
	client, err := getClusterClient()
	if err != nil {
		if err == errNoCluster {
			return nil
		}
		return err
	}
	m := nodeContainerManager{client: client}
	return servicecommon.UpgradeNodeContainer(&m, name, pool, writer)
}

func (p *kubernetesProvisioner) RemoveNodeContainer(name string, pool string, writer io.Writer) error {
	client, err := getClusterClient()
	if err != nil {
		if err == errNoCluster {
			return nil
		}
		return err
	}
	return cleanupDaemonSet(client, name, pool)
}

func (p *kubernetesProvisioner) Shell(opts provision.ShellOptions) error {
	return execCommand(execOpts{
		app:    opts.App,
		unit:   opts.Unit,
		cmds:   []string{"/usr/bin/env", "TERM=" + opts.Term, "bash", "-l"},
		stdout: opts.Conn,
		stderr: opts.Conn,
		stdin:  opts.Conn,
		termSize: &term.Size{
			Width:  uint16(opts.Width),
			Height: uint16(opts.Height),
		},
		tty: true,
	})
}

func (p *kubernetesProvisioner) ExecuteCommand(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	client, err := getClusterClient()
	if err != nil {
		return err
	}
	l, err := provision.ServiceLabels(provision.ServiceLabelsOpts{
		App:         app,
		Provisioner: provisionerName,
		Prefix:      tsuruLabelPrefix,
	})
	if err != nil {
		return errors.WithStack(err)
	}
	pods, err := client.Core().Pods(tsuruNamespace).List(v1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set(l.ToAppSelector())).String(),
	})
	if err != nil {
		return errors.WithStack(err)
	}
	if len(pods.Items) == 0 {
		return provision.ErrEmptyApp
	}
	for _, pod := range pods.Items {
		err = execCommand(execOpts{
			unit:   pod.Name,
			app:    app,
			cmds:   append([]string{"/bin/bash", "-lc", cmd}, args...),
			stdout: stdout,
			stderr: stderr,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *kubernetesProvisioner) ExecuteCommandOnce(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	return execCommand(execOpts{
		app:    app,
		cmds:   append([]string{"/bin/bash", "-lc", cmd}, args...),
		stdout: stdout,
		stderr: stderr,
	})
}

func (p *kubernetesProvisioner) ExecuteCommandIsolated(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	// TODO(cezarsa)
	return errors.New("not implemented yet")
}
