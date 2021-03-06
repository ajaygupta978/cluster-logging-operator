package functional

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/ViaQ/logerr/log"
	"github.com/openshift/cluster-logging-operator/internal/pkg/generator/forwarder"
	logging "github.com/openshift/cluster-logging-operator/pkg/apis/logging/v1"
	"github.com/openshift/cluster-logging-operator/pkg/certificates"
	"github.com/openshift/cluster-logging-operator/pkg/components/fluentd"
	"github.com/openshift/cluster-logging-operator/pkg/constants"
	"github.com/openshift/cluster-logging-operator/pkg/utils"
	"github.com/openshift/cluster-logging-operator/test"
	"github.com/openshift/cluster-logging-operator/test/client"
	"github.com/openshift/cluster-logging-operator/test/helpers/cmd"
	"github.com/openshift/cluster-logging-operator/test/helpers/oc"
	"github.com/openshift/cluster-logging-operator/test/runtime"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	applicationLog     = "application"
	auditLog           = "audit"
	k8sAuditLog        = "k8s"
	ApplicationLogFile = "/tmp/app-logs"
)

var fluentdLogPath = map[string]string{
	applicationLog: "/var/log/containers",
	auditLog:       "/var/log/audit",
	k8sAuditLog:    "/var/log/kube-apiserver",
}

var outputLogFile = map[string]string{
	applicationLog: ApplicationLogFile,
	auditLog:       "/tmp/audit-logs",
	k8sAuditLog:    "/tmp/audit-logs",
}

var (
	maxDuration          time.Duration
	defaultRetryInterval time.Duration
)

type receiverBuilder func(f *FluentdFunctionalFramework, b *runtime.PodBuilder, output logging.OutputSpec) error

//FluentdFunctionalFramework deploys stand alone fluentd with the fluent.conf as generated by input ClusterLogForwarder CR
type FluentdFunctionalFramework struct {
	Name              string
	Namespace         string
	Conf              string
	image             string
	labels            map[string]string
	Forwarder         *logging.ClusterLogForwarder
	Test              *client.Test
	pod               *corev1.Pod
	fluentContainerId string
	receiverBuilders  []receiverBuilder
	closeClient       func()
}

func init() {
	maxDuration, _ = time.ParseDuration("2m")
	defaultRetryInterval, _ = time.ParseDuration("1ms")
}

func NewFluentdFunctionalFramework() *FluentdFunctionalFramework {
	test := client.NewTest()
	return NewFluentdFunctionalFrameworkUsing(client.NewTest(), test.Close, 0)
}

func NewFluentdFunctionalFrameworkUsing(t *client.Test, fnClose func(), verbosity int) *FluentdFunctionalFramework {
	if level, found := os.LookupEnv("LOG_LEVEL"); found {
		if i, err := strconv.Atoi(level); err == nil {
			verbosity = i
		}
	}

	log.MustInit("functional-framework")
	log.SetLogLevel(verbosity)
	testName := "functional"
	framework := &FluentdFunctionalFramework{
		Name:      testName,
		Namespace: t.NS.Name,
		image:     utils.GetComponentImage(constants.FluentdName),
		labels: map[string]string{
			"testtype": "functional",
			"testname": testName,
		},
		Test:             t,
		Forwarder:        runtime.NewClusterLogForwarder(),
		receiverBuilders: []receiverBuilder{},
		closeClient:      fnClose,
	}
	return framework
}

func NewCRIOLogMessage(timestamp, message string, partial bool) string {
	fullOrPartial := "F"
	if partial {
		fullOrPartial = "P"
	}
	return fmt.Sprintf("%s stdout %s %s $n", timestamp, fullOrPartial, message)
}

func (f *FluentdFunctionalFramework) Cleanup() {
	f.Test.Close()
}

func (f *FluentdFunctionalFramework) RunCommand(container string, cmd ...string) (string, error) {
	log.V(2).Info("Running", "container", container, "cmd", cmd)
	out, err := runtime.ExecOc(f.pod, strings.ToLower(container), cmd[0], cmd[1:]...)
	log.V(2).Info("Exec'd", "out", out, "err", err)
	return out, err
}

//Deploy the objects needed to functional Test
func (f *FluentdFunctionalFramework) Deploy() (err error) {
	visitors := []runtime.PodBuilderVisitor{
		func(b *runtime.PodBuilder) error {
			return f.addOutputContainers(b, f.Forwarder.Spec.Outputs)
		},
	}
	return f.DeployWithVisitors(visitors)
}
func (f *FluentdFunctionalFramework) DeployWithVisitor(visitor runtime.PodBuilderVisitor) (err error) {
	visitors := []runtime.PodBuilderVisitor{
		visitor,
	}
	return f.DeployWithVisitors(visitors)
}

//Deploy the objects needed to functional Test
func (f *FluentdFunctionalFramework) DeployWithVisitors(visitors []runtime.PodBuilderVisitor) (err error) {
	log.V(2).Info("Generating config", "forwarder", f.Forwarder)
	clfYaml, _ := yaml.Marshal(f.Forwarder)
	if f.Conf, err = forwarder.Generate(string(clfYaml), false, false); err != nil {
		return err
	}
	log.V(2).Info("Generating Certificates")
	if err, _ = certificates.GenerateCertificates(f.Test.NS.Name,
		utils.GetScriptsDir(), "elasticsearch",
		utils.DefaultWorkingDir); err != nil {
		return err
	}
	log.V(2).Info("Creating config configmap")
	configmap := runtime.NewConfigMap(f.Test.NS.Name, f.Name, map[string]string{})
	runtime.NewConfigMapBuilder(configmap).
		Add("fluent.conf", f.Conf).
		Add("run.sh", fluentd.RunScript)
	if err = f.Test.Client.Create(configmap); err != nil {
		return err
	}

	log.V(2).Info("Creating certs configmap")
	certsName := "certs-" + f.Name
	certs := runtime.NewConfigMap(f.Test.NS.Name, certsName, map[string]string{})
	runtime.NewConfigMapBuilder(certs).
		Add("tls.key", string(utils.GetWorkingDirFileContents("system.logging.fluentd.key"))).
		Add("tls.crt", string(utils.GetWorkingDirFileContents("system.logging.fluentd.crt")))
	if err = f.Test.Client.Create(certs); err != nil {
		return err
	}

	log.V(2).Info("Creating service")
	service := runtime.NewService(f.Test.NS.Name, f.Name)
	runtime.NewServiceBuilder(service).
		AddServicePort(24231, 24231).
		WithSelector(f.labels)
	if err = f.Test.Client.Create(service); err != nil {
		return err
	}

	role := runtime.NewRole(f.Test.NS.Name, f.Name,
		v1.PolicyRule{
			Verbs:     []string{"list", "get"},
			Resources: []string{"pods", "namespaces"},
			APIGroups: []string{""},
		},
	)
	if err = f.Test.Client.Create(role); err != nil {
		return err
	}
	rolebinding := runtime.NewRoleBinding(f.Test.NS.Name, f.Name,
		v1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     role.Name,
		},
		v1.Subject{
			Kind: "ServiceAccount",
			Name: "default",
		},
	)
	if err = f.Test.Client.Create(rolebinding); err != nil {
		return err
	}

	log.V(2).Info("Defining pod...")
	var containers []corev1.Container
	f.pod = runtime.NewPod(f.Test.NS.Name, f.Name, containers...)
	b := runtime.NewPodBuilder(f.pod).
		WithLabels(f.labels).
		AddConfigMapVolume("config", f.Name).
		AddConfigMapVolume("entrypoint", f.Name).
		AddConfigMapVolume("certs", certsName).
		AddContainer(constants.FluentdName, f.image).
		AddEnvVar("LOG_LEVEL", "debug").
		AddEnvVarFromFieldRef("POD_IP", "status.podIP").
		AddVolumeMount("config", "/etc/fluent/configs.d/user", "", true).
		AddVolumeMount("entrypoint", "/opt/app-root/src/run.sh", "run.sh", true).
		AddVolumeMount("certs", "/etc/fluent/metrics", "", true).
		End()
	for _, visit := range visitors {
		if err = visit(b); err != nil {
			return err
		}
	}
	log.V(2).Info("Creating pod", "pod", f.pod)
	if err = f.Test.Client.Create(f.pod); err != nil {
		return err
	}

	log.V(2).Info("waiting for pod to be ready")
	if err = oc.Literal().From("oc wait -n %s pod/%s --timeout=120s --for=condition=Ready", f.Test.NS.Name, f.Name).Output(); err != nil {
		return err
	}
	if err = f.Test.Client.Get(f.pod); err != nil {
		return err
	}
	log.V(2).Info("waiting for service endpoints to be ready")
	err = wait.PollImmediate(time.Second*2, time.Second*10, func() (bool, error) {
		ips, err := oc.Get().WithNamespace(f.Test.NS.Name).Resource("endpoints", f.Name).OutputJsonpath("{.subsets[*].addresses[*].ip}").Run()
		if err != nil {
			return false, nil
		}
		// if there are IPs in the service endpoint, the service is available
		if strings.TrimSpace(ips) != "" {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("service could not be started")
	}
	log.V(2).Info("waiting for fluentd to be ready")
	err = wait.PollImmediate(time.Second*2, time.Second*30, func() (bool, error) {
		output, err := oc.Literal().From("oc logs -n %s pod/%s %s", f.Test.NS.Name, f.Name, constants.FluentdName).Run()
		if err != nil {
			return false, nil
		}

		// if fluentd started successfully return success
		if strings.Contains(output, "flush_thread actually running") {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("fluentd did not start in the container")
	}
	for _, cs := range f.pod.Status.ContainerStatuses {
		if cs.Name == constants.FluentdName {
			f.fluentContainerId = strings.TrimPrefix(cs.ContainerID, "cri-o://")
			break
		}
	}
	return nil
}

func (f *FluentdFunctionalFramework) addOutputContainers(b *runtime.PodBuilder, outputs []logging.OutputSpec) error {
	log.V(2).Info("Adding outputs", "outputs", outputs)
	for _, output := range outputs {
		if output.Type == logging.OutputTypeFluentdForward {
			if err := f.addForwardOutput(b, output); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *FluentdFunctionalFramework) WaitForPodToBeReady() error {
	return oc.Literal().From(fmt.Sprintf("oc wait -n %s pod/%s --timeout=60s --for=condition=Ready", f.Test.NS.Name, f.Name)).Output()
}

func (f *FluentdFunctionalFramework) WriteMessagesToApplicationLog(msg string, numOfLogs int) error {
	filename := fmt.Sprintf("%s/%s_%s_%s-%s.log", fluentdLogPath[applicationLog], f.pod.Name, f.pod.Namespace, constants.FluentdName, f.fluentContainerId)
	return f.WriteMessagesToLog(msg, numOfLogs, filename)
}

func (f *FluentdFunctionalFramework) WriteMessagesToAuditLog(msg string, numOfLogs int) error {
	filename := fmt.Sprintf("%s/audit.log", fluentdLogPath[auditLog])
	return f.WriteMessagesToLog(msg, numOfLogs, filename)
}

func (f *FluentdFunctionalFramework) WriteMessagesTok8sAuditLog(msg string, numOfLogs int) error {
	filename := fmt.Sprintf("%s/audit.log", fluentdLogPath[k8sAuditLog])
	return f.WriteMessagesToLog(msg, numOfLogs, filename)
}

func (f *FluentdFunctionalFramework) WritesApplicationLogs(numOfLogs uint64) error {
	return f.WritesNApplicationLogsOfSize(numOfLogs, uint64(100))
}

func (f *FluentdFunctionalFramework) WritesNApplicationLogsOfSize(numOfLogs, size uint64) error {
	msg := "$(date -u +'%Y-%m-%dT%H:%M:%S.%N%:z') stdout F $msg "
	//podname_ns_containername-containerid.log
	//functional_testhack-16511862744968_fluentd-90a0f0a7578d254eec640f08dd155cc2184178e793d0289dff4e7772757bb4f8.log
	filepath := fmt.Sprintf("/var/log/containers/%s_%s_%s-%s.log", f.pod.Name, f.pod.Namespace, constants.FluentdName, f.fluentContainerId)
	result, err := f.RunCommand(constants.FluentdName, "bash", "-c", fmt.Sprintf("bash -c 'mkdir -p /var/log/containers;echo > %s;msg=$(cat /dev/urandom|tr -dc 'a-zA-Z0-9'|fold -w %d|head -n 1);for n in $(seq 1 %d);do echo %s >> %s; done'", filepath, size, numOfLogs, msg, filepath))
	log.V(3).Info("FluentdFunctionalFramework.WritesNApplicationLogsOfSize", "result", result, "err", err)
	return err
}

func (f *FluentdFunctionalFramework) WriteMessagesToLog(msg string, numOfLogs int, filename string) error {
	logPath := filepath.Dir(filename)
	cmd := fmt.Sprintf("bash -c 'mkdir -p %s;for n in {1..%d};do echo \"%s\" >> %s;done'", logPath, numOfLogs, msg, filename)
	result, err := f.RunCommand(constants.FluentdName, "bash", "-c", cmd)
	log.V(3).Info("FluentdFunctionalFramework.WriteMessagesToLog", "result", result, "err", err)
	return err
}

func (f *FluentdFunctionalFramework) ReadApplicationLogsFrom(outputName string) (result string, err error) {
	return f.ReadLogsFrom(outputName, applicationLog)
}

func (f *FluentdFunctionalFramework) ReadAuditLogsFrom(outputName string) (result string, err error) {
	return f.ReadLogsFrom(outputName, auditLog)
}

func (f *FluentdFunctionalFramework) Readk8sAuditLogsFrom(outputName string) (result string, err error) {
	return f.ReadLogsFrom(outputName, k8sAuditLog)
}

func (f *FluentdFunctionalFramework) ReadLogsFrom(outputName string, outputLogType string) (result string, err error) {
	file, ok := outputLogFile[outputLogType]
	if !ok {
		return "", fmt.Errorf(fmt.Sprintf("can't find log of type %s", outputLogType))
	}
	err = wait.PollImmediate(defaultRetryInterval, maxDuration, func() (done bool, err error) {
		result, err = f.RunCommand(outputName, "cat", file)
		if result != "" && err == nil {
			return true, nil
		}
		log.V(4).Error(err, "Polling logs")
		return false, nil
	})
	if err == nil {
		result = fmt.Sprintf("[%s]", strings.Join(strings.Split(strings.TrimSpace(result), "\n"), ","))
	}
	log.V(4).Info("Returning", "logs", result)
	return result, err
}

func (f *FluentdFunctionalFramework) ReadNApplicationLogsFrom(n uint64, outputName string) ([]string, error) {
	lines := []string{}
	ctx, cancel := context.WithTimeout(context.Background(), test.SuccessTimeout())
	defer cancel()
	reader, err := cmd.TailReaderForContainer(f.pod, outputName, ApplicationLogFile)
	if err != nil {
		log.V(3).Error(err, "Error creating tail reader")
		return nil, err
	}
	for {
		line, err := reader.ReadLineContext(ctx)
		if err != nil {
			log.V(3).Error(err, "Error readlinecontext")
			return nil, err
		}
		lines = append(lines, line)
		n--
		if n == 0 {
			break
		}
	}
	return lines, err
}
