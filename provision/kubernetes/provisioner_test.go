// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package kubernetes

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/url"
	"sort"

	"github.com/fsouza/go-dockerclient"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/image"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/nodecontainer"
	"github.com/tsuru/tsuru/provision/provisiontest"
	"github.com/tsuru/tsuru/safe"
	"gopkg.in/check.v1"
	"k8s.io/client-go/pkg/api/v1"
	extensions "k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/kubernetes/pkg/util/term"
)

func (s *S) TestListNodes(c *check.C) {
	s.mockfakeNodes(c)
	nodes, err := s.p.ListNodes([]string{})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 3)
	c.Assert(nodes[0].Address(), check.Equals, "https://clusteraddr")
	c.Assert(nodes[1].Address(), check.Equals, "192.168.99.1")
	c.Assert(nodes[2].Address(), check.Equals, "192.168.99.2")
}

func (s *S) TestListNodesWithoutNodes(c *check.C) {
	nodes, err := s.p.ListNodes([]string{})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 0)
}

func (s *S) TestListNodesFilteringByAddress(c *check.C) {
	s.mockfakeNodes(c)
	nodes, err := s.p.ListNodes([]string{"192.168.99.1"})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 1)
}

func (s *S) TestAddNodeCluster(c *check.C) {
	url := "https://clusteraddr"
	opts := provision.AddNodeOptions{
		Address: url,
		Metadata: map[string]string{
			"cluster": "true",
		},
	}
	err := s.p.AddNode(opts)
	c.Assert(err, check.IsNil)
	cli, err := getClusterClient()
	c.Assert(err, check.IsNil)
	c.Assert(cli, check.DeepEquals, s.client)
	c.Assert(s.lastConf.Host, check.Equals, url)
}

func (s *S) TestRemoveNode(c *check.C) {
	s.mockfakeNodes(c)
	opts := provision.RemoveNodeOptions{
		Address: "192.168.99.1",
	}
	err := s.p.RemoveNode(opts)
	c.Assert(err, check.IsNil)
	nodes, err := s.p.ListNodes([]string{})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 2)
}

func (s *S) TestRemoveNodeCluster(c *check.C) {
	s.mockfakeNodes(c)
	opts := provision.RemoveNodeOptions{
		Address: "https://clusteraddr",
	}
	err := s.p.RemoveNode(opts)
	c.Assert(err, check.IsNil)
	nodes, err := s.p.ListNodes([]string{})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 0)
}

func (s *S) TestRemoveNodeNotFound(c *check.C) {
	s.mockfakeNodes(c)
	opts := provision.RemoveNodeOptions{
		Address: "192.168.99.99",
	}
	err := s.p.RemoveNode(opts)
	c.Assert(err, check.Equals, provision.ErrNodeNotFound)
}

func (s *S) TestRemoveNodeWithRebalance(c *check.C) {
	s.mockfakeNodes(c)
	_, err := s.client.Core().Pods(tsuruNamespace).Create(&v1.Pod{
		ObjectMeta: v1.ObjectMeta{Name: "p1", Namespace: tsuruNamespace},
	})
	c.Assert(err, check.IsNil)
	evictionCalled := false
	s.client.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		if action.GetSubresource() == "eviction" {
			evictionCalled = true
			return true, action.(ktesting.CreateAction).GetObject(), nil
		}
		return
	})
	opts := provision.RemoveNodeOptions{
		Address:   "192.168.99.1",
		Rebalance: true,
	}
	err = s.p.RemoveNode(opts)
	c.Assert(err, check.IsNil)
	nodes, err := s.p.ListNodes([]string{})
	c.Assert(err, check.IsNil)
	c.Assert(nodes, check.HasLen, 2)
	c.Assert(evictionCalled, check.Equals, true)
}

func (s *S) TestUnits(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web":    "python myapp.py",
			"worker": "myworker",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.Start(a, "")
	c.Assert(err, check.IsNil)
	wait()
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.DeepEquals, []provision.Unit{
		{
			ID:          "myapp-web-pod-1-1",
			Name:        "myapp-web-pod-1-1",
			AppName:     "myapp",
			ProcessName: "web",
			Type:        "python",
			Ip:          "192.168.99.1",
			Status:      "started",
			Address:     &url.URL{Scheme: "http", Host: "192.168.99.1:30000"},
		},
		{
			ID:          "myapp-worker-pod-2-1",
			Name:        "myapp-worker-pod-2-1",
			AppName:     "myapp",
			ProcessName: "worker",
			Type:        "python",
			Ip:          "192.168.99.1",
			Status:      "started",
			Address:     &url.URL{Scheme: "http", Host: "192.168.99.1"},
		},
	})
}

func (s *S) TestUnitsEmpty(c *check.C) {
	s.mockfakeNodes(c)
	a := &app.App{Name: "myapp", TeamOwner: s.team.Name}
	err := app.CreateApp(a, s.user)
	c.Assert(err, check.IsNil)
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
}

func (s *S) TestGetNode(c *check.C) {
	s.mockfakeNodes(c)
	host := "192.168.99.1"
	node, err := s.p.GetNode(host)
	c.Assert(err, check.IsNil)
	c.Assert(node.Address(), check.Equals, host)
	node, err = s.p.GetNode("http://doesnotexist.com")
	c.Assert(err, check.NotNil)
	c.Assert(err, check.Equals, provision.ErrNodeNotFound)
	c.Assert(node, check.IsNil)
}

func (s *S) TestGetNodeWithoutCluster(c *check.C) {
	_, err := s.p.GetNode("anything")
	c.Assert(err, check.Equals, provision.ErrNodeNotFound)
}

func (s *S) TestRegisterUnit(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	err = s.p.RegisterUnit(a, units[0].ID, nil)
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	c.Assert(a.HasBind(&units[0]), check.Equals, true)
}

func (s *S) TestRegisterUnitDeployUnit(c *check.C) {
	a, _, rollback := s.defaultReactions(c)
	defer rollback()
	podID, err := createBuildJob(buildJobParams{
		client:           s.client,
		app:              a,
		sourceImage:      "myimg",
		destinationImage: "destimg",
	})
	c.Assert(err, check.IsNil)
	err = s.p.RegisterUnit(a, podID, map[string]interface{}{
		"processes": map[string]interface{}{
			"web":    "w1",
			"worker": "w2",
		},
	})
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	meta, err := image.GetImageCustomData("destimg")
	c.Assert(err, check.IsNil)
	c.Assert(meta, check.DeepEquals, image.ImageMetadata{
		Name:            "destimg",
		CustomData:      map[string]interface{}{},
		LegacyProcesses: map[string]string{},
		Processes: map[string][]string{
			"web":    {"w1"},
			"worker": {"w2"},
		},
	})
}

func (s *S) TestAddUnits(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
}

func (s *S) TestRemoveUnits(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 3, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 3)
	err = s.p.RemoveUnits(a, 2, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
}

func (s *S) TestRestart(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	id := units[0].ID
	err = s.p.Restart(a, "", nil)
	c.Assert(err, check.IsNil)
	wait()
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
	c.Assert(units[0].ID, check.Not(check.Equals), id)
}

func (s *S) TestStopStart(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	err = s.p.Stop(a, "")
	c.Assert(err, check.IsNil)
	wait()
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 0)
	err = s.p.Start(a, "")
	c.Assert(err, check.IsNil)
	wait()
	units, err = s.p.Units(a)
	c.Assert(err, check.IsNil)
	c.Assert(units, check.HasLen, 1)
}

func (s *S) TestProvisionerDestroy(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	data := []byte("archivedata")
	archive := ioutil.NopCloser(bytes.NewReader(data))
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	_, err = s.p.UploadDeploy(a, archive, int64(len(data)), false, evt)
	c.Assert(err, check.IsNil)
	wait()
	err = s.p.Destroy(a)
	c.Assert(err, check.IsNil)
	deps, err := s.client.Extensions().Deployments(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(deps.Items, check.HasLen, 0)
	pods, err := s.client.Core().Pods(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(pods.Items, check.HasLen, 0)
	replicas, err := s.client.Extensions().ReplicaSets(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(replicas.Items, check.HasLen, 0)
	services, err := s.client.Core().Services(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(services.Items, check.HasLen, 0)
}

func (s *S) TestProvisionerDestroyNothingToDo(c *check.C) {
	s.mockfakeNodes(c)
	a := provisiontest.NewFakeApp("myapp", "plat", 0)
	err := s.p.Destroy(a)
	c.Assert(err, check.IsNil)
}

func (s *S) TestProvisionerRoutableAddresses(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	data := []byte("archivedata")
	archive := ioutil.NopCloser(bytes.NewReader(data))
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	_, err = s.p.UploadDeploy(a, archive, int64(len(data)), false, evt)
	c.Assert(err, check.IsNil)
	wait()
	addrs, err := s.p.RoutableAddresses(a)
	c.Assert(err, check.IsNil)
	c.Assert(addrs, check.DeepEquals, []url.URL{
		{
			Scheme: "http",
			Host:   "192.168.99.1:30000",
		},
		{
			Scheme: "http",
			Host:   "192.168.99.2:30000",
		},
	})
}

func (s *S) TestUploadDeploy(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	data := []byte("archivedata")
	archive := ioutil.NopCloser(bytes.NewReader(data))
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	img, err := s.p.UploadDeploy(a, archive, int64(len(data)), false, evt)
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	c.Assert(img, check.Equals, "tsuru/app-myapp:v1")
	wait()
	deps, err := s.client.Extensions().Deployments(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(deps.Items, check.HasLen, 2)
	var depNames []string
	for _, dep := range deps.Items {
		depNames = append(depNames, dep.Name)
	}
	sort.Strings(depNames)
	c.Assert(depNames, check.DeepEquals, []string{"myapp-web", "myapp-worker"})
	units, err := s.p.Units(a)
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	c.Assert(units, check.HasLen, 2)
}

func (s *S) TestUpgradeNodeContainer(c *check.C) {
	s.mockfakeNodes(c)
	c1 := nodecontainer.NodeContainerConfig{
		Name: "bs",
		Config: docker.Config{
			Image: "bsimg",
		},
		HostConfig: docker.HostConfig{
			RestartPolicy: docker.AlwaysRestart(),
			Privileged:    true,
			Binds:         []string{"/xyz:/abc:ro"},
		},
	}
	err := nodecontainer.AddNewContainer("", &c1)
	c.Assert(err, check.IsNil)
	c2 := c1
	c2.Config.Env = []string{"e1=v1"}
	err = nodecontainer.AddNewContainer("p1", &c2)
	c.Assert(err, check.IsNil)
	c3 := c1
	err = nodecontainer.AddNewContainer("p2", &c3)
	c.Assert(err, check.IsNil)
	buf := &bytes.Buffer{}
	err = s.p.UpgradeNodeContainer("bs", "", buf)
	c.Assert(err, check.IsNil)
	daemons, err := s.client.Extensions().DaemonSets(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(daemons.Items, check.HasLen, 3)
}

func (s *S) TestRemoveNodeContainer(c *check.C) {
	s.mockfakeNodes(c)
	_, err := s.client.Extensions().DaemonSets(tsuruNamespace).Create(&extensions.DaemonSet{
		ObjectMeta: v1.ObjectMeta{
			Name:      "node-container-bs-pool-p1",
			Namespace: tsuruNamespace,
		},
	})
	c.Assert(err, check.IsNil)
	_, err = s.client.Core().Pods(tsuruNamespace).Create(&v1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "node-container-bs-pool-p1-xyz",
			Namespace: tsuruNamespace,
			Labels: map[string]string{
				"tsuru.io/is-tsuru":            "true",
				"tsuru.io/is-node-container":   "true",
				"tsuru.io/provisioner":         provisionerName,
				"tsuru.io/node-container-name": "bs",
				"tsuru.io/node-container-pool": "p1",
			},
		},
	})
	c.Assert(err, check.IsNil)
	err = s.p.RemoveNodeContainer("bs", "p1", ioutil.Discard)
	c.Assert(err, check.IsNil)
	daemons, err := s.client.Extensions().DaemonSets(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(daemons.Items, check.HasLen, 0)
	pods, err := s.client.Core().Pods(tsuruNamespace).List(v1.ListOptions{})
	c.Assert(err, check.IsNil)
	c.Assert(pods.Items, check.HasLen, 0)
}

func (s *S) TestShell(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	buf := safe.NewBuffer([]byte("echo test"))
	conn := &provisiontest.FakeConn{Buf: buf}
	err = s.p.Shell(provision.ShellOptions{
		App:    a,
		Conn:   conn,
		Width:  99,
		Height: 42,
	})
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	rollback()
	c.Assert(s.stream.stdin, check.Equals, "echo test")
	var sz term.Size
	err = json.Unmarshal([]byte(s.stream.resize), &sz)
	c.Assert(err, check.IsNil)
	c.Assert(sz, check.DeepEquals, term.Size{Width: 99, Height: 42})
	c.Assert(s.stream.urls, check.HasLen, 1)
	c.Assert(s.stream.urls[0].Path, check.DeepEquals, "/api/v1/namespaces/default/pods/myapp-web-pod-1-1/exec")
}

func (s *S) TestShellSpecificUnit(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 2, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	buf := safe.NewBuffer([]byte("echo test"))
	conn := &provisiontest.FakeConn{Buf: buf}
	err = s.p.Shell(provision.ShellOptions{
		App:    a,
		Conn:   conn,
		Width:  99,
		Height: 42,
		Unit:   "myapp-web-pod-2-2",
	})
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	rollback()
	c.Assert(s.stream.stdin, check.Equals, "echo test")
	var sz term.Size
	err = json.Unmarshal([]byte(s.stream.resize), &sz)
	c.Assert(err, check.IsNil)
	c.Assert(sz, check.DeepEquals, term.Size{Width: 99, Height: 42})
	c.Assert(s.stream.urls, check.HasLen, 1)
	c.Assert(s.stream.urls[0].Path, check.DeepEquals, "/api/v1/namespaces/default/pods/myapp-web-pod-2-2/exec")
}

func (s *S) TestShellSpecificUnitNotFound(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 1, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	buf := safe.NewBuffer([]byte("echo test"))
	conn := &provisiontest.FakeConn{Buf: buf}
	err = s.p.Shell(provision.ShellOptions{
		App:    a,
		Conn:   conn,
		Width:  99,
		Height: 42,
		Unit:   "invalid-unit",
	})
	c.Assert(err, check.DeepEquals, &provision.UnitNotFoundError{ID: "invalid-unit"})
}

func (s *S) TestShellNoUnits(c *check.C) {
	a, _, rollback := s.defaultReactions(c)
	defer rollback()
	buf := safe.NewBuffer([]byte("echo test"))
	conn := &provisiontest.FakeConn{Buf: buf}
	err := s.p.Shell(provision.ShellOptions{
		App:    a,
		Conn:   conn,
		Width:  99,
		Height: 42,
	})
	c.Assert(err, check.Equals, provision.ErrEmptyApp)
}

func (s *S) TestExecuteCommand(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 2, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	stdout, stderr := safe.NewBuffer(nil), safe.NewBuffer(nil)
	err = s.p.ExecuteCommand(stdout, stderr, a, "mycmd", "arg1", "arg2")
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	rollback()
	c.Assert(stdout.String(), check.Equals, "stdout datastdout data")
	c.Assert(stderr.String(), check.Equals, "stderr datastderr data")
	c.Assert(s.stream.urls, check.HasLen, 2)
	c.Assert(s.stream.urls[0].Path, check.DeepEquals, "/api/v1/namespaces/default/pods/myapp-web-pod-1-1/exec")
	c.Assert(s.stream.urls[1].Path, check.DeepEquals, "/api/v1/namespaces/default/pods/myapp-web-pod-2-2/exec")
}

func (s *S) TestExecuteCommandOnce(c *check.C) {
	a, wait, rollback := s.defaultReactions(c)
	defer rollback()
	imgName := "myapp:v1"
	err := image.SaveImageCustomData(imgName, map[string]interface{}{
		"processes": map[string]interface{}{
			"web": "python myapp.py",
		},
	})
	c.Assert(err, check.IsNil)
	err = image.AppendAppImageName(a.GetName(), imgName)
	c.Assert(err, check.IsNil)
	err = s.p.AddUnits(a, 2, "web", nil)
	c.Assert(err, check.IsNil)
	wait()
	stdout, stderr := safe.NewBuffer(nil), safe.NewBuffer(nil)
	err = s.p.ExecuteCommandOnce(stdout, stderr, a, "mycmd", "arg1", "arg2")
	c.Assert(err, check.IsNil, check.Commentf("%+v", err))
	rollback()
	c.Assert(stdout.String(), check.Equals, "stdout data")
	c.Assert(stderr.String(), check.Equals, "stderr data")
	c.Assert(s.stream.urls, check.HasLen, 1)
	c.Assert(s.stream.urls[0].Path, check.DeepEquals, "/api/v1/namespaces/default/pods/myapp-web-pod-1-1/exec")
}
