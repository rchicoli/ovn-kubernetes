package cni

// contains code for cnishim - one that gets called as the cni Plugin
// This does not do the real cni work. This is just the client to the cniserver
// that does the real work.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Plugin is the structure to hold the endpoint information and the corresponding
// functions to use it
type Plugin struct {
	socketPath string
}

// NewCNIPlugin creates the internal Plugin object
func NewCNIPlugin(socketPath string) *Plugin {
	if len(socketPath) == 0 {
		socketPath = serverSocketPath
	}
	return &Plugin{socketPath: socketPath}
}

// Create and fill a Request with this Plugin's environment and stdin which
// contain the CNI variables and configuration
func newCNIRequest(args *skel.CmdArgs) *Request {
	envMap := make(map[string]string)
	for _, item := range os.Environ() {
		idx := strings.Index(item, "=")
		if idx > 0 {
			envMap[strings.TrimSpace(item[:idx])] = item[idx+1:]
		}
	}

	return &Request{
		Env:    envMap,
		Config: args.StdinData,
	}
}

// Send a CNI request to the CNI server via JSON + HTTP over a root-owned unix socket,
// and return the result
func (p *Plugin) doCNI(url string, req interface{}) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CNI request %v: %v", req, err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(proto, addr string) (net.Conn, error) {
				return net.Dial("unix", p.socketPath)
			},
		},
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to send CNI request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read CNI result: %v", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("CNI request failed with status %v: '%s'", resp.StatusCode, string(body))
	}

	return body, nil
}

func setupLogging(conf *ovntypes.NetConf) {
	var err error
	var level klog.Level

	if conf.LogLevel != "" {
		if err = level.Set(conf.LogLevel); err != nil {
			klog.Warningf("Failed to set klog log level to %s: %v", conf.LogLevel, err)
		}
	}
	if conf.LogFile != "" {
		klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
		klog.InitFlags(klogFlags)
		if err := klogFlags.Set("logtostderr", "false"); err != nil {
			klog.Warningf("Error setting klog logtostderr: %v", err)
		}
		if err := klogFlags.Set("alsologtostderr", "true"); err != nil {
			klog.Warningf("Error setting klog alsologtostderr: %v", err)
		}
		klog.SetOutput(&lumberjack.Logger{
			Filename:   conf.LogFile,
			MaxSize:    conf.LogFileMaxSize, // megabytes
			MaxBackups: conf.LogFileMaxBackups,
			MaxAge:     conf.LogFileMaxAge, // days
			Compress:   true,
		})
	}
}

// report the CNI request processing time to CNI server. This is used for the cni_request_duration_seconds metrics
func (p *Plugin) postMetrics(startTime time.Time, cmd command, err error) {
	elapsedTime := time.Since(startTime).Seconds()
	_, _ = p.doCNI("http://dummy/metrics", &CNIRequestMetrics{
		Command:     cmd,
		ElapsedTime: elapsedTime,
		HasErr:      err != nil,
	})
}

func shimClientsetFromConfig(auth *KubeAPIAuth) (*shimClientset, error) {
	if auth.Kubeconfig == "" && auth.KubeAPIServer == "" {
		return nil, nil
	}

	var caData []byte
	var err error
	if auth.KubeCAData != "" {
		caData, err = base64.StdEncoding.DecodeString(auth.KubeCAData)
		if err != nil {
			return nil, fmt.Errorf("failed to decode Kube API CA data: %v", err)
		}
	}
	kubeconfig := &config.KubernetesConfig{
		Kubeconfig: auth.Kubeconfig,
		APIServer:  auth.KubeAPIServer,
		Token:      auth.KubeAPIToken,
		TokenFile:  auth.KubeAPITokenFile,
		CAData:     caData,
	}

	kclient, err := util.NewKubernetesClientset(kubeconfig)
	if err != nil {
		return nil, err
	}

	return &shimClientset{
		kclient: kclient,
	}, nil
}

type shimClientset struct {
	PodInfoGetter
	kclient kubernetes.Interface
}

func (c *shimClientset) getPod(namespace, name string) (*kapi.Pod, error) {
	return c.kclient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
}

func initPodRequestUnprivilegedMode(req *Request, isDPUHostMode bool) (*PodRequest, error) {
	var vsClient libovsdbclient.Client

	if !isDPUHostMode {
		// Initialize OVS exec runner; find binaries that the CNI code uses.
		if err := SetExec(kexec.New()); err != nil {
			return nil, fmt.Errorf("failed to initialize OVS exec runner: %w", err)
		}
	}

	vsClient, err := libovsdb.NewVSwitchClient(make(chan struct{}))
	if err != nil {
		return nil, fmt.Errorf("failed to create vswitchd database client: %w", err)
	}

	pr, err := cniRequestToPodRequest(req, vsClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create pod request: %w", err)
	}

	return pr, nil
}

func (p *Plugin) cmdCommon(args *skel.CmdArgs, detail string) (*Response, *Request, string, error) {
	// read the config stdin args to obtain cniVersion
	conf, err := config.ReadCNIConfig(args.StdinData)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%s: invalid stdin args %w", detail, err)
	}
	setupLogging(conf)

	req := newCNIRequest(args)
	body, err := p.doCNI("http://dummy/", req)
	if err != nil {
		return nil, nil, "", err
	}

	response := &Response{}
	if err = json.Unmarshal(body, response); err != nil {
		return nil, nil, "", fmt.Errorf("%s: failed to unmarshal response '%s': %v", detail, string(body), err)
	}

	return response, req, conf.CNIVersion, nil
}

// CmdAdd is the callback for 'add' cni calls from skel
func (p *Plugin) CmdAdd(args *skel.CmdArgs) error {
	var err, errR error

	startTime := time.Now()
	defer func() {
		p.postMetrics(startTime, CNIAdd, err)
		if err != nil {
			klog.Errorf(err.Error())
		}
	}()

	response, req, cniVersion, errC := p.cmdCommon(args, "ADD")
	if err != nil {
		err = errC
		return err
	}

	clientset, errK := shimClientsetFromConfig(response.KubeAuth)
	if errK != nil {
		err = errK
		return err
	}

	var result *current.Result
	if response.Result != nil {
		// Return the full CNI result from ovnkube-node if it configured the pod interface
		result = response.Result
	} else {
		// The onvkube-node is running in un-privileged mode. The responsibility of
		// plugging an interface into Pod is on the Shim.

		pr, errP := initPodRequestUnprivilegedMode(req, response.PodIFInfo.IsDPUHostMode)
		if errP != nil {
			err = errP
			return err
		}
		defer pr.cancel()

		// In the case where ovnkube-node is running in Unprivileged mode,
		// use the IPAM details from ovnkube-node to configure the pod interface
		result, errR = pr.getCNIResult(clientset, response.PodIFInfo)
		if errR != nil {
			err = fmt.Errorf("failed to get CNI Result from pod interface info %v: %w", response.PodIFInfo, errR)
			return err
		}
	}

	return types.PrintResult(result, cniVersion)
}

// CmdDel is the callback for 'teardown' cni calls from skel
func (p *Plugin) CmdDel(args *skel.CmdArgs) error {
	var err error

	startTime := time.Now()
	defer func() {
		p.postMetrics(startTime, CNIDel, err)
		if err != nil {
			klog.Errorf(err.Error())
		}
	}()

	response, req, _, errC := p.cmdCommon(args, "DEL")
	if err != nil {
		err = errC
		return err
	}

	// if Result is nil, then ovnkube-node is running in unprivileged mode so unconfigure the Interface from here.
	if response.Result == nil {
		pr, errP := initPodRequestUnprivilegedMode(req, response.PodIFInfo.IsDPUHostMode)
		if errP != nil {
			err = errP
			return err
		}
		defer pr.cancel()

		err = pr.UnconfigureInterface(response.PodIFInfo)
	}
	return err
}

// CmdCheck is the callback for 'checking' container's networking is as expected.
func (p *Plugin) CmdCheck(args *skel.CmdArgs) error {
	// noop...CMD check is not considered useful, and has a considerable performance impact
	// to pod bring up times with CRIO. This is due to the fact that CRIO currently calls check
	// after CNI ADD before it finishes bringing the container up
	return nil
}
