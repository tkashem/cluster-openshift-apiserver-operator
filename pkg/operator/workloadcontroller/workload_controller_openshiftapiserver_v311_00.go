package workloadcontroller

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ghodss/yaml"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	appsclientv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"

	openshiftapi "github.com/openshift/api"
	openshiftcontrolplanev1 "github.com/openshift/api/openshiftcontrolplane/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	openshiftconfigclientv1 "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	"github.com/openshift/cluster-openshift-apiserver-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-openshift-apiserver-operator/pkg/operator/v311_00_assets"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehash"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// syncOpenShiftAPIServer_v311_00_to_latest takes care of synchronizing (not upgrading) the thing we're managing.
// most of the time the sync method will be good for a large span of minor versions
func syncOpenShiftAPIServer_v311_00_to_latest(c OpenShiftAPIServerOperator, originalOperatorConfig *operatorv1.OpenShiftAPIServer) (bool, error) {
	errors := []error{}
	operatorConfig := originalOperatorConfig.DeepCopy()

	directResourceResults := resourceapply.ApplyDirectly(c.kubeClient, c.eventRecorder, v311_00_assets.Asset,
		"v3.11.0/openshift-apiserver/ns.yaml",
		"v3.11.0/openshift-apiserver/apiserver-clusterrolebinding.yaml",
		"v3.11.0/openshift-apiserver/svc.yaml",
		"v3.11.0/openshift-apiserver/sa.yaml",
		"v3.11.0/openshift-apiserver/trusted_ca_cm.yaml",
	)
	for _, currResult := range directResourceResults {
		if currResult.Error != nil {
			errors = append(errors, fmt.Errorf("%q (%T): %v", currResult.File, currResult.Type, currResult.Error))
			continue
		}
	}

	_, _, err := manageOpenShiftAPIServerConfigMap_v311_00_to_latest(c.kubeClient, c.kubeClient.CoreV1(), c.eventRecorder, operatorConfig)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "configmap", err))
	}

	_, _, err = manageOpenShiftAPIServerImageImportCA_v311_00_to_latest(c.openshiftConfigClient, c.kubeClient.CoreV1(), c.eventRecorder)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "image-import-ca", err))
	}

	// our configmaps and secrets are in order, now it is time to create the DS
	// TODO check basic preconditions here
	actualDaemonSet, _, err := manageOpenShiftAPIServerDaemonSet_v311_00_to_latest(c.kubeClient, c.kubeClient.AppsV1(), c.eventRecorder, c.targetImagePullSpec, c.operatorImagePullSpec, operatorConfig, operatorConfig.Status.Generations)
	if err != nil {
		errors = append(errors, fmt.Errorf("%q: %v", "daemonsets", err))
	}

	// manage status
	availableConditionReasons := []string{}
	availableConditionMessages := []string{}
	switch {
	case actualDaemonSet == nil:
		availableConditionReasons = append(availableConditionReasons, "NoDaemon")
		availableConditionMessages = append(availableConditionMessages, "daemonset/apiserver.openshift-apiserver: could not be retrieved")
	case actualDaemonSet.Status.NumberAvailable == 0:
		availableConditionReasons = append(availableConditionReasons, "NoAPIServerPod")
		availableConditionMessages = append(availableConditionMessages, "no openshift-apiserver daemon pods available on any node.")
	}
	sort.Sort(sort.StringSlice(availableConditionReasons))

	switch {
	case len(availableConditionMessages) == 1:
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeAvailable,
			Status:  operatorv1.ConditionFalse,
			Reason:  availableConditionReasons[0],
			Message: availableConditionMessages[0],
		})
	case len(availableConditionMessages) > 1:
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeAvailable,
			Status:  operatorv1.ConditionFalse,
			Reason:  strings.Join(availableConditionReasons, "\n"),
			Message: strings.Join(availableConditionMessages, "\n"),
		})
	default:
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeAvailable,
			Status: operatorv1.ConditionTrue,
		})
	}

	// If the daemonset is up to date and the operatorConfig are up to date, then we are no longer progressing
	var progressingMessages []string
	if actualDaemonSet != nil && actualDaemonSet.ObjectMeta.Generation != actualDaemonSet.Status.ObservedGeneration {
		progressingMessages = append(progressingMessages, fmt.Sprintf("daemonset/apiserver.openshift-operator: observed generation is %d, desired generation is %d.", actualDaemonSet.Status.ObservedGeneration, actualDaemonSet.ObjectMeta.Generation))
	}
	if operatorConfig.ObjectMeta.Generation != operatorConfig.Status.ObservedGeneration {
		progressingMessages = append(progressingMessages, fmt.Sprintf("openshiftapiserveroperatorconfigs/instance: observed generation is %d, desired generation is %d.", operatorConfig.Status.ObservedGeneration, operatorConfig.ObjectMeta.Generation))
	}

	if len(progressingMessages) == 0 {
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeProgressing,
			Status: operatorv1.ConditionFalse,
		})
	} else {
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeProgressing,
			Status:  operatorv1.ConditionTrue,
			Reason:  "DesiredStateNotYetAchieved",
			Message: strings.Join(progressingMessages, "\n"),
		})
	}

	// TODO this is changing too early and it was before too.
	operatorConfig.Status.ObservedGeneration = operatorConfig.ObjectMeta.Generation
	resourcemerge.SetDaemonSetGeneration(&operatorConfig.Status.Generations, actualDaemonSet)
	if len(errors) > 0 {
		message := ""
		for _, err := range errors {
			message = message + err.Error() + "\n"
		}
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
			Type:    workloadDegradedCondition,
			Status:  operatorv1.ConditionTrue,
			Message: message,
			Reason:  "SyncError",
		})
	} else {
		v1helpers.SetOperatorCondition(&operatorConfig.Status.Conditions, operatorv1.OperatorCondition{
			Type:   workloadDegradedCondition,
			Status: operatorv1.ConditionFalse,
		})
	}

	// if we are available, we need to try to set our versions correctly.
	if v1helpers.IsOperatorConditionTrue(operatorConfig.Status.Conditions, operatorv1.OperatorStatusTypeAvailable) {
		// we have the actual daemonset and we need the pull spec
		operandVersion := status.VersionForOperand(
			operatorclient.OperatorNamespace,
			actualDaemonSet.Spec.Template.Spec.Containers[0].Image,
			c.kubeClient.CoreV1(),
			c.eventRecorder)
		c.versionRecorder.SetVersion("openshift-apiserver", operandVersion)

	}
	if !equality.Semantic.DeepEqual(operatorConfig.Status, originalOperatorConfig.Status) {
		if _, err := c.operatorConfigClient.OpenShiftAPIServers().UpdateStatus(operatorConfig); err != nil {
			return false, err
		}
	}

	if len(errors) > 0 {
		return true, nil
	}
	if !v1helpers.IsOperatorConditionFalse(operatorConfig.Status.Conditions, operatorv1.OperatorStatusTypeDegraded) {
		return true, nil
	}
	if !v1helpers.IsOperatorConditionFalse(operatorConfig.Status.Conditions, operatorv1.OperatorStatusTypeProgressing) {
		return true, nil
	}
	if !v1helpers.IsOperatorConditionTrue(operatorConfig.Status.Conditions, operatorv1.OperatorStatusTypeAvailable) {
		return true, nil
	}

	return false, nil
}

// mergeImageRegistryCertificates merges two distinct ConfigMap, both containing
// trusted CAs for Image Registries. The first one is the default CA bundle for
// OpenShift internal registry access, the latter is a custom config map that may
// be configured by the user on image.config.openshift.io/cluster.
func mergeImageRegistryCertificates(cfgCli openshiftconfigclientv1.ConfigV1Interface, cli coreclientv1.CoreV1Interface) (map[string]string, error) {
	cas := make(map[string]string)

	internalRegistryCAs, err := cli.ConfigMaps("openshift-image-registry").Get(
		"image-registry-certificates", metav1.GetOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	} else if err == nil {
		for key, value := range internalRegistryCAs.Data {
			cas[key] = value
		}
	}

	imageConfig, err := cfgCli.Images().Get(
		"cluster", metav1.GetOptions{},
	)
	if err != nil {
		return nil, err
	}

	// No custom config map, return.
	if len(imageConfig.Spec.AdditionalTrustedCA.Name) == 0 {
		return cas, nil
	}

	additionalImageRegistryCAs, err := cli.ConfigMaps(
		operatorclient.GlobalUserSpecifiedConfigNamespace,
	).Get(
		imageConfig.Spec.AdditionalTrustedCA.Name,
		metav1.GetOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	} else if err == nil {
		for key, value := range additionalImageRegistryCAs.Data {
			cas[key] = value
		}
	}
	return cas, nil
}

// manageOpenShiftAPIServerImageImportCA_v311_00_to_latest synchronizes image import ca-bundle. Returns the modified
// ca-bundle ConfigMap.
func manageOpenShiftAPIServerImageImportCA_v311_00_to_latest(openshiftConfigClient openshiftconfigclientv1.ConfigV1Interface, client coreclientv1.CoreV1Interface, recorder events.Recorder) (*corev1.ConfigMap, bool, error) {
	mergedCAs, err := mergeImageRegistryCertificates(openshiftConfigClient, client)
	if err != nil {
		return nil, false, err
	}
	requiredConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: operatorclient.TargetNamespace,
			Name:      imageImportCAName,
		},
		Data: mergedCAs,
	}

	// this can leave configmaps mounted without any content, but that should not have an impact on functionality since empty and missing
	// should logically be treated the same in the case of trust.
	return resourceapply.ApplyConfigMap(client, recorder, requiredConfigMap)
}

func manageOpenShiftAPIServerConfigMap_v311_00_to_latest(kubeClient kubernetes.Interface, client coreclientv1.ConfigMapsGetter, recorder events.Recorder, operatorConfig *operatorv1.OpenShiftAPIServer) (*corev1.ConfigMap, bool, error) {
	configMap := resourceread.ReadConfigMapV1OrDie(v311_00_assets.MustAsset("v3.11.0/openshift-apiserver/cm.yaml"))
	defaultConfig := v311_00_assets.MustAsset("v3.11.0/openshift-apiserver/defaultconfig.yaml")
	requiredConfigMap, _, err := resourcemerge.MergePrunedConfigMap(
		&openshiftcontrolplanev1.OpenShiftAPIServerConfig{},
		configMap,
		"config.yaml",
		nil,
		defaultConfig,
		operatorConfig.Spec.ObservedConfig.Raw,
		operatorConfig.Spec.UnsupportedConfigOverrides.Raw,
	)
	if err != nil {
		return nil, false, err
	}

	return resourceapply.ApplyConfigMap(client, recorder, requiredConfigMap)
}

func loglevelToKlog(logLevel operatorv1.LogLevel) string {
	switch logLevel {
	case operatorv1.Normal:
		return "2"
	case operatorv1.Debug:
		return "4"
	case operatorv1.Trace:
		return "6"
	case operatorv1.TraceAll:
		return "8"
	default:
		return "2"
	}
}

func manageOpenShiftAPIServerDaemonSet_v311_00_to_latest(
	kubeClient kubernetes.Interface,
	client appsclientv1.DaemonSetsGetter,
	recorder events.Recorder,
	imagePullSpec string,
	operatorImagePullSpec string,
	operatorConfig *operatorv1.OpenShiftAPIServer,
	generationStatus []operatorv1.GenerationStatus,
) (*appsv1.DaemonSet, bool, error) {
	tmpl := v311_00_assets.MustAsset("v3.11.0/openshift-apiserver/ds.yaml")

	r := strings.NewReplacer(
		"${IMAGE}", imagePullSpec,
		"${OPERATOR_IMAGE}", operatorImagePullSpec,
		"${REVISION}", strconv.Itoa(int(operatorConfig.Status.LatestAvailableRevision)),
		"${VERBOSITY}", loglevelToKlog(operatorConfig.Spec.LogLevel),
	)
	tmpl = []byte(r.Replace(string(tmpl)))

	re := regexp.MustCompile("\\$\\{[^}]*}")
	if match := re.Find(tmpl); len(match) > 0 {
		return nil, false, fmt.Errorf("invalid template reference %q", string(match))
	}

	required := resourceread.ReadDaemonSetV1OrDie(tmpl)

	// we set this so that when the requested image pull spec changes, we always have a diff.  Remember that we don't directly
	// diff any fields on the daemonset because they can be rewritten by admission and we don't want to constantly be fighting
	// against admission or defaults.  That was a problem with original versions of apply.
	if required.Annotations == nil {
		required.Annotations = map[string]string{}
	}
	required.Annotations["openshiftapiservers.operator.openshift.io/pull-spec"] = imagePullSpec
	required.Annotations["openshiftapiservers.operator.openshift.io/operator-pull-spec"] = operatorImagePullSpec

	required.Labels["revision"] = strconv.Itoa(int(operatorConfig.Status.LatestAvailableRevision))
	required.Spec.Template.Labels["revision"] = strconv.Itoa(int(operatorConfig.Status.LatestAvailableRevision))

	var observedConfig map[string]interface{}
	if err := yaml.Unmarshal(operatorConfig.Spec.ObservedConfig.Raw, &observedConfig); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal the observedConfig: %v", err)
	}
	proxyConfig, _, err := unstructured.NestedStringMap(observedConfig, "workloadcontroller", "proxy")
	if err != nil {
		return nil, false, fmt.Errorf("couldn't get the proxy config from observedConfig: %v", err)
	}

	proxyEnvVars := proxyMapToEnvVars(proxyConfig)
	for i, container := range required.Spec.Template.Spec.Containers {
		required.Spec.Template.Spec.Containers[i].Env = append(container.Env, proxyEnvVars...)
	}

	// we watch some resources so that our daemonset will redeploy without explicitly and carefully ordered resource creation
	inputHashes, err := resourcehash.MultipleObjectHashStringMapForObjectReferences(
		kubeClient,
		resourcehash.NewObjectRef().ForConfigMap().InNamespace(operatorclient.TargetNamespace).Named("config"),
		resourcehash.NewObjectRef().ForSecret().InNamespace(operatorclient.TargetNamespace).Named("etcd-client"),
		resourcehash.NewObjectRef().ForConfigMap().InNamespace(operatorclient.TargetNamespace).Named("etcd-serving-ca"),
		resourcehash.NewObjectRef().ForConfigMap().InNamespace(operatorclient.TargetNamespace).Named("image-import-ca"),
		resourcehash.NewObjectRef().ForConfigMap().InNamespace(operatorclient.TargetNamespace).Named("trusted-ca-bundle"),
	)
	if err != nil {
		return nil, false, fmt.Errorf("invalid dependency reference: %q", err)
	}
	inputHashes["desired.generation"] = fmt.Sprintf("%d", operatorConfig.ObjectMeta.Generation)
	for k, v := range inputHashes {
		annotationKey := fmt.Sprintf("operator.openshift.io/dep-%s", k)
		required.Annotations[annotationKey] = v
		if required.Spec.Template.Annotations == nil {
			required.Spec.Template.Annotations = map[string]string{}
		}
		required.Spec.Template.Annotations[annotationKey] = v
	}

	return resourceapply.ApplyDaemonSet(client, recorder, required, resourcemerge.ExpectedDaemonSetGeneration(required, generationStatus), false)
}

var openshiftScheme = runtime.NewScheme()

func init() {
	if err := openshiftapi.Install(openshiftScheme); err != nil {
		panic(err)
	}
}

func resourceSelectorForCLI(obj runtime.Object) string {
	groupVersionKind := obj.GetObjectKind().GroupVersionKind()
	if len(groupVersionKind.Kind) == 0 {
		if kinds, _, _ := scheme.Scheme.ObjectKinds(obj); len(kinds) > 0 {
			groupVersionKind = kinds[0]
		}
	}
	if len(groupVersionKind.Kind) == 0 {
		if kinds, _, _ := openshiftScheme.ObjectKinds(obj); len(kinds) > 0 {
			groupVersionKind = kinds[0]
		}
	}
	if len(groupVersionKind.Kind) == 0 {
		groupVersionKind = schema.GroupVersionKind{Kind: "Unknown"}
	}
	kind := groupVersionKind.Kind
	group := groupVersionKind.Group
	var name string
	accessor, err := meta.Accessor(obj)
	if err != nil {
		name = "unknown"
	}
	name = accessor.GetName()
	if len(group) > 0 {
		group = "." + group
	}
	return kind + group + "/" + name
}

func proxyMapToEnvVars(proxyConfig map[string]string) []corev1.EnvVar {
	if proxyConfig == nil {
		return nil
	}

	envVars := []corev1.EnvVar{}
	for k, v := range proxyConfig {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	// sort the env vars to prevent update hotloops
	sort.Slice(envVars, func(i, j int) bool { return envVars[i].Name < envVars[j].Name })
	return envVars
}
