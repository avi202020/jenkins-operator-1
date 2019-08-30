package openshift

import (
	"fmt"
	appsV1Api "github.com/openshift/api/apps/v1"
	routeV1Api "github.com/openshift/api/route/v1"
	appsV1client "github.com/openshift/client-go/apps/clientset/versioned/typed/apps/v1"
	routeV1Client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	"github.com/pkg/errors"
	"jenkins-operator/pkg/apis/v2/v1alpha1"
	jenkinsDefaultSpec "jenkins-operator/pkg/service/jenkins/spec"
	"jenkins-operator/pkg/service/platform/helper"
	"jenkins-operator/pkg/service/platform/kubernetes"
	coreV1Api "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("platform")

// OpenshiftService struct for Openshift platform service
type OpenshiftService struct {
	kubernetes.K8SService

	appClient   appsV1client.AppsV1Client
	routeClient routeV1Client.RouteV1Client
}

// Init initializes OpenshiftService
func (service *OpenshiftService) Init(config *rest.Config, scheme *runtime.Scheme) error {
	err := service.K8SService.Init(config, scheme)
	if err != nil {
		return errors.Wrap(err, "Failed to init K8S platform service")
	}

	appClient, err := appsV1client.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "Failed to init apps V1 client for Openshift")
	}
	service.appClient = *appClient

	routeClient, err := routeV1Client.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "Failed to init route V1 client for Openshift")
	}
	service.routeClient = *routeClient

	return nil
}

// GetRoute returns Route object and connection protocol from Openshift
func (service OpenshiftService) GetRoute(namespace string, name string) (*routeV1Api.Route, string, error) {
	route, err := service.routeClient.Routes(namespace).Get(name, metav1.GetOptions{})
	if err != nil && k8serrors.IsNotFound(err) {
		return nil, "", errors.New(fmt.Sprintf("Route %v in namespace %v not found", name, namespace))
	} else if err != nil {
		return nil, "", err
	}

	var routeScheme = "http"
	if route.Spec.TLS.Termination != "" {
		routeScheme = "https"
	}
	return route, routeScheme, nil
}

// CreateDeployConf - creates deployment config for Jenkins instance
func (service OpenshiftService) CreateDeployConf(instance v1alpha1.Jenkins) error {

	activeDeadlineSecond := int64(21600)
	terminationGracePeriod := int64(30)
	nexusRoute, routeScheme, err := service.GetRoute(instance.Namespace, instance.Name)
	if err != nil {
		return err
	}

	jenkinsUiUrl := fmt.Sprintf("%v://%v", routeScheme, nexusRoute.Spec.Host)

	// Can't assign pointer to constant, that is why — create an intermediate var.
	timeout := jenkinsDefaultSpec.JenkinsRecreateTimeout
	command := []string{"sh", "-c", fmt.Sprintf(
		"if [ -d /var/lib/jenkins/.ssh/ ]; then cd /var/lib/jenkins/.ssh/;" +
			" for file in config id_rsa jenkins-slave-id_rsa;" +
			" do if [ -f $file ]; then chmod 400 $file; fi; done; fi;")}

	labels := helper.GenerateLabels(instance.Name)
	jenkinsDcObject := &appsV1Api.DeploymentConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: appsV1Api.DeploymentConfigSpec{
			Replicas: 1,
			Triggers: []appsV1Api.DeploymentTriggerPolicy{
				{
					Type: appsV1Api.DeploymentTriggerOnConfigChange,
				},
			},
			Strategy: appsV1Api.DeploymentStrategy{
				Type: appsV1Api.DeploymentStrategyTypeRecreate,
				RecreateParams: &appsV1Api.RecreateDeploymentStrategyParams{
					TimeoutSeconds: &timeout,
				},
				ActiveDeadlineSeconds: &activeDeadlineSecond,
			},
			Selector: labels,
			Template: &coreV1Api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: coreV1Api.PodSpec{
					SecurityContext:               &coreV1Api.PodSecurityContext{},
					RestartPolicy:                 coreV1Api.RestartPolicyAlways,
					DeprecatedServiceAccount:      instance.Name,
					DNSPolicy:                     coreV1Api.DNSClusterFirst,
					TerminationGracePeriodSeconds: &terminationGracePeriod,
					SchedulerName:                 coreV1Api.DefaultSchedulerName,
					InitContainers: []coreV1Api.Container{
						{
							Image:                    "busybox",
							ImagePullPolicy:          coreV1Api.PullIfNotPresent,
							Name:                     "grant-permissions",
							Command:                  command,
							TerminationMessagePath:   "/dev/termination-log",
							TerminationMessagePolicy: coreV1Api.TerminationMessageReadFile,
						},
					},
					Containers: []coreV1Api.Container{
						{
							Name:            instance.Name,
							Image:           instance.Spec.Image + ":" + instance.Spec.Version,
							ImagePullPolicy: coreV1Api.PullAlways,
							Env: []coreV1Api.EnvVar{
								{
									Name:  "OPENSHIFT_ENABLE_OAUTH",
									Value: "false",
								},
								{
									Name:  "OPENSHIFT_ENABLE_REDIRECT_PROMPT",
									Value: "true",
								},
								{
									Name:  "KUBERNETES_MASTER",
									Value: "https://kubernetes.default:443",
								},
								{
									Name:  "KUBERNETES_TRUST_CERTIFICATES",
									Value: "true",
								},
								{
									Name:  "JNLP_SERVICE_NAME",
									Value: fmt.Sprintf("%v-jnlp", instance.Name),
								},
								{
									Name: "JENKINS_PASSWORD",
									ValueFrom: &coreV1Api.EnvVarSource{
										SecretKeyRef: &coreV1Api.SecretKeySelector{
											LocalObjectReference: coreV1Api.LocalObjectReference{
												Name: fmt.Sprintf("%v-%v", instance.Name, jenkinsDefaultSpec.JenkinsPasswordSecretName),
											},
											Key: "password",
										},
									},
								},
								{
									Name:  "JENKINS_UI_URL",
									Value: jenkinsUiUrl,
								},
							},
							SecurityContext: nil,
							Ports: []coreV1Api.ContainerPort{
								{
									ContainerPort: jenkinsDefaultSpec.JenkinsDefaultUiPort,
									Protocol:      coreV1Api.ProtocolTCP,
								},
							},

							ReadinessProbe: &coreV1Api.Probe{
								TimeoutSeconds:      10,
								InitialDelaySeconds: 60,
								SuccessThreshold:    1,
								PeriodSeconds:       10,
								FailureThreshold:    3,
								Handler: coreV1Api.Handler{
									HTTPGet: &coreV1Api.HTTPGetAction{
										Path:   "/login",
										Port:   intstr.FromInt(jenkinsDefaultSpec.JenkinsDefaultUiPort),
										Scheme: coreV1Api.URISchemeHTTP,
									},
								},
							},

							VolumeMounts: []coreV1Api.VolumeMount{
								{
									MountPath:        "/var/lib/jenkins",
									Name:             fmt.Sprintf("%v-jenkins-data", instance.Name),
									ReadOnly:         false,
									SubPath:          "",
									MountPropagation: nil,
								},
							},
							TerminationMessagePath:   "/dev/termination-log",
							TerminationMessagePolicy: coreV1Api.TerminationMessageReadFile,
							Resources: coreV1Api.ResourceRequirements{
								Requests: map[coreV1Api.ResourceName]resource.Quantity{
									coreV1Api.ResourceMemory: resource.MustParse(jenkinsDefaultSpec.JenkinsDefaultMemoryRequest),
								},
							},
						},
					},
					ServiceAccountName: instance.Name,
					Volumes: []coreV1Api.Volume{
						{
							Name: fmt.Sprintf("%v-jenkins-data", instance.Name),
							VolumeSource: coreV1Api.VolumeSource{
								PersistentVolumeClaim: &coreV1Api.PersistentVolumeClaimVolumeSource{
									ClaimName: fmt.Sprintf("%v-data", instance.Name),
								},
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(&instance, jenkinsDcObject, service.Scheme); err != nil {
		return err
	}

	jenkinsDc, err := service.appClient.DeploymentConfigs(jenkinsDcObject.Namespace).Get(jenkinsDcObject.Name, metav1.GetOptions{})
	if err != nil && k8serrors.IsNotFound(err) {
		log.V(1).Info(fmt.Sprintf("Creating a new DeploymentConfig %s/%s for Jenkins %s", jenkinsDcObject.Namespace, jenkinsDcObject.Name, instance.Name))

		jenkinsDc, err = service.appClient.DeploymentConfigs(jenkinsDcObject.Namespace).Create(jenkinsDcObject)
		if err != nil {
			return err
		}

		log.Info(fmt.Sprintf("DeploymentConfig %s/%s has been created", jenkinsDc.Namespace, jenkinsDc.Name))
	} else if err != nil {
		return err
	} else if !apiequality.Semantic.DeepEqual(jenkinsDc.Spec, jenkinsDcObject.Spec) {
		jenkinsDc.Spec = jenkinsDcObject.Spec
		_, err = service.appClient.DeploymentConfigs(jenkinsDc.Namespace).Update(jenkinsDc)
		if err != nil {
			return errors.Wrapf(err, "Failed to update DeploymentConfig %v !", jenkinsDcObject.Name)
		}
		log.Info(fmt.Sprintf("DeploymentConfig %s/%s has been updated!", jenkinsDc.Namespace, jenkinsDc.Name))
	}

	return nil
}

func (service OpenshiftService) CreateExternalEndpoint(instance v1alpha1.Jenkins) error {

	labels := helper.GenerateLabels(instance.Name)

	routeObject := &routeV1Api.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: routeV1Api.RouteSpec{
			TLS: &routeV1Api.TLSConfig{
				Termination:                   routeV1Api.TLSTerminationEdge,
				InsecureEdgeTerminationPolicy: routeV1Api.InsecureEdgeTerminationPolicyRedirect,
			},
			To: routeV1Api.RouteTargetReference{
				Name: instance.Name,
				Kind: "Service",
			},
			Port: &routeV1Api.RoutePort{
				TargetPort: intstr.IntOrString{IntVal: jenkinsDefaultSpec.JenkinsDefaultUiPort},
			},
		},
	}

	if err := controllerutil.SetControllerReference(&instance, routeObject, service.Scheme); err != nil {
		return err
	}

	route, err := service.routeClient.Routes(routeObject.Namespace).Get(routeObject.Name, metav1.GetOptions{})
	if err != nil && k8serrors.IsNotFound(err) {
		route, err = service.routeClient.Routes(routeObject.Namespace).Create(routeObject)
		if err != nil {
			return err
		}
		log.Info(fmt.Sprintf("Route %s/%s has been created", route.Namespace, route.Name))
	} else if err != nil {
		return err
	} else if !reflect.DeepEqual(routeObject.Spec, route.Spec) {

		route.Spec = routeObject.Spec
		_, err = service.routeClient.Routes(routeObject.Namespace).Update(route)
		if err != nil {
			return errors.Wrapf(err, "Failed to update DeploymentConfig %v !", routeObject.Name)
		}
	}

	return nil
}