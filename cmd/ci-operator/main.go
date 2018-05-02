package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"time"

	coreapi "k8s.io/api/core/v1"
	rbacapi "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	coreclientset "k8s.io/client-go/kubernetes/typed/core/v1"
	rbacclientset "k8s.io/client-go/kubernetes/typed/rbac/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	imageapi "github.com/openshift/api/image/v1"
	projectapi "github.com/openshift/api/project/v1"
	routeapi "github.com/openshift/api/route/v1"
	imageclientset "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	"github.com/openshift/client-go/project/clientset/versioned"
	routeclientset "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"

	"github.com/openshift/ci-operator/pkg/api"
	"github.com/openshift/ci-operator/pkg/steps"
)

func bindOptions() *options {
	opt := &options{}
	flag.StringVar(&opt.namespace, "namespace", "", "Namespace to create builds into, defaults to build_id from JOB_SPEC")
	flag.StringVar(&opt.baseNamespace, "base-namespace", "stable", "Namespace to read builds from, defaults to stable.")
	flag.StringVar(&opt.rawBuildConfig, "build-config", "", "Configuration for the build to run, as JSON.")
	flag.BoolVar(&opt.dry, "dry-run", true, "Do not contact the API server.")
	flag.StringVar(&opt.writeParams, "write-params", "", "If set write an env-compatible file with the output of the job.")
	flag.DurationVar(&opt.idleCleanupDuration, "delete-when-idle", opt.idleCleanupDuration, "If no pod is running for longer than this interval, delete the namespace.")
	return opt
}

type options struct {
	rawBuildConfig string
	dry            bool
	writeParams    string

	namespace           string
	baseNamespace       string
	idleCleanupDuration time.Duration

	buildConfig   *api.ReleaseBuildConfiguration
	jobSpec       *steps.JobSpec
	clusterConfig *rest.Config
}

func (o *options) Validate() error {
	if o.rawBuildConfig == "" {
		return fmt.Errorf("job configuration must be provided with `--build-config`")
	}
	return nil
}

func (o *options) Complete() error {
	jobSpec, err := steps.ResolveSpecFromEnv()
	if err != nil {
		return fmt.Errorf("failed to resolve job spec: %v", err)
	}

	if len(o.namespace) == 0 {
		o.namespace = "ci-op-{id}"
	}
	o.namespace = strings.Replace(o.namespace, "{id}", jobSpec.Hash(), -1)

	jobSpec.SetNamespace(o.namespace)
	jobSpec.SetBaseNamespace(o.baseNamespace)

	o.jobSpec = jobSpec

	if err := json.Unmarshal([]byte(o.rawBuildConfig), &o.buildConfig); err != nil {
		return fmt.Errorf("malformed build configuration: %v", err)
	}

	clusterConfig, err := loadClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to load cluster config: %v", err)
	}
	o.clusterConfig = clusterConfig

	return nil
}

// loadClusterConfig loads connection configuration
// for the cluster we're deploying to. We prefer to
// use in-cluster configuration if possible, but will
// fall back to using default rules otherwise.
func loadClusterConfig() (*rest.Config, error) {
	clusterConfig, err := rest.InClusterConfig()
	if err == nil {
		return clusterConfig, nil
	}

	credentials, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return nil, fmt.Errorf("could not load credentials from config: %v", err)
	}

	clusterConfig, err = clientcmd.NewDefaultClientConfig(*credentials, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("could not load client configuration: %v", err)
	}
	return clusterConfig, nil
}

func (o *options) Run() error {
	start := time.Now()
	defer func() {
		log.Printf("Ran for %s", time.Now().Sub(start).Truncate(time.Second))
	}()
	var is *imageapi.ImageStream
	if !o.dry {
		projectGetter, err := versioned.NewForConfig(o.clusterConfig)
		if err != nil {
			return fmt.Errorf("could not get project client for cluster config: %v", err)
		}

		log.Printf("Creating namespace %s", o.namespace)
		for {
			project, err := projectGetter.ProjectV1().ProjectRequests().Create(&projectapi.ProjectRequest{
				ObjectMeta: meta.ObjectMeta{
					Name: o.namespace,
				},
			})
			if err != nil && !errors.IsAlreadyExists(err) {
				return fmt.Errorf("could not set up namespace for test: %v", err)
			}
			if err != nil {
				project, err = projectGetter.ProjectV1().Projects().Get(o.namespace, meta.GetOptions{})
				if err != nil {
					if errors.IsNotFound(err) {
						continue
					}
					return fmt.Errorf("cannot retrieve test namespace: %v", err)
				}
			}
			if project.Status.Phase == coreapi.NamespaceTerminating {
				log.Println("Waiting for namespace to finish terminating before creating another")
				time.Sleep(3 * time.Second)
				continue
			}
			break
		}

		if o.idleCleanupDuration > 0 {
			if err := o.createNamespaceCleanupPod(); err != nil {
				return err
			}
		}

		imageGetter, err := imageclientset.NewForConfig(o.clusterConfig)
		if err != nil {
			return fmt.Errorf("could not get image client for cluster config: %v", err)
		}

		// create the image stream or read it to get its uid
		is, err = imageGetter.ImageStreams(o.jobSpec.Namespace()).Create(&imageapi.ImageStream{
			ObjectMeta: meta.ObjectMeta{
				Namespace: o.jobSpec.Namespace(),
				Name:      steps.PipelineImageStream,
			},
		})
		if err != nil {
			if !errors.IsAlreadyExists(err) {
				return fmt.Errorf("could not set up pipeline imagestream for test: %v", err)
			}
			is, _ = imageGetter.ImageStreams(o.jobSpec.Namespace()).Get(steps.PipelineImageStream, meta.GetOptions{})
		}
		if is != nil {
			isTrue := true
			o.jobSpec.SetOwner(&meta.OwnerReference{
				APIVersion: "image.openshift.io/v1",
				Kind:       "ImageStream",
				Name:       steps.PipelineImageStream,
				UID:        is.UID,
				Controller: &isTrue,
			})
		}
	}

	buildSteps, err := steps.FromConfig(o.buildConfig, o.jobSpec, o.clusterConfig)
	if err != nil {
		return fmt.Errorf("failed to generate steps from config: %v", err)
	}

	if err := steps.Run(api.BuildGraph(buildSteps), o.dry); err != nil {
		return err
	}

	if len(o.writeParams) > 0 {
		if err := o.writeParameters(o.writeParams, is); err != nil {
			return fmt.Errorf("failed to write parameters: %v", err)
		}
	}

	return nil
}

func admittedRoute(route *routeapi.Route) (string, bool) {
	for _, ingress := range route.Status.Ingress {
		if len(ingress.Host) == 0 {
			continue
		}
		for _, condition := range ingress.Conditions {
			if condition.Type == routeapi.RouteAdmitted && condition.Status == coreapi.ConditionTrue {
				return ingress.Host, true
			}
		}
	}
	return "", false
}

func main() {
	opt := bindOptions()
	flag.Parse()

	if err := opt.Validate(); err != nil {
		fmt.Printf("Invalid options: %v\n", err)
		os.Exit(1)
	}

	if err := opt.Complete(); err != nil {
		fmt.Printf("Invalid environment: %v\n", err)
		os.Exit(1)
	}

	if err := opt.Run(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}

func (o *options) writeParameters(path string, is *imageapi.ImageStream) error {
	log.Printf("Writing parameters to %s", path)
	var params []string

	params = append(params, fmt.Sprintf("JOB_NAME=%q", o.jobSpec.Job))
	params = append(params, fmt.Sprintf("NAMESPACE=%q", o.namespace))

	if tagConfig := o.buildConfig.ReleaseTagConfiguration; tagConfig != nil {
		registry := "REGISTRY"
		if is != nil {
			if len(is.Status.PublicDockerImageRepository) > 0 {
				registry = strings.SplitN(is.Status.PublicDockerImageRepository, "/", 2)[0]
			} else if len(is.Status.DockerImageRepository) > 0 {
				registry = strings.SplitN(is.Status.DockerImageRepository, "/", 2)[0]
			}
		}
		var format string
		if len(tagConfig.Name) > 0 {
			format = fmt.Sprintf("%s/%s/%s:%s", registry, o.namespace, fmt.Sprintf("%s%s", tagConfig.NamePrefix, steps.StableImageStream), "${component}")
		} else {
			format = fmt.Sprintf("%s/%s/%s:%s", registry, o.namespace, fmt.Sprintf("%s${component}", tagConfig.NamePrefix), tagConfig.Tag)
		}
		params = append(params, fmt.Sprintf("IMAGE_FORMAT='%s'", strings.Replace(strings.Replace(format, "\\", "\\\\", -1), "'", "\\'", -1)))
	}

	if len(o.buildConfig.RpmBuildCommands) > 0 {
		if o.dry {
			params = append(params, "RPM_REPO=\"\"")
		} else {
			routeclient, err := routeclientset.NewForConfig(o.clusterConfig)
			if err != nil {
				return fmt.Errorf("could not get route client for cluster config: %v", err)
			}
			if err := wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
				route, err := routeclient.Routes(o.namespace).Get(steps.RPMRepoName, meta.GetOptions{})
				if err != nil {
					return false, err
				}
				if host, ok := admittedRoute(route); ok {
					params = append(params, fmt.Sprintf("RPM_REPO=%q", host))
					return true, nil
				}
				return false, nil
			}); err != nil {
				return err
			}
		}
	}

	if o.dry {
		log.Printf("\n%s", strings.Join(params, "\n"))
	} else {
		params = append(params, "")
		if err := ioutil.WriteFile(path, []byte(strings.Join(params, "\n")), 0640); err != nil {
			return err
		}
	}
	return nil
}

// createNamespaceCleanupPod creates a pod that deletes the job namespace if no other run-once pods are running
// for more than idleCleanupDuration.
func (o *options) createNamespaceCleanupPod() error {
	log.Printf("Namespace will be deleted after %s of idle time", o.idleCleanupDuration)
	client, err := coreclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get image client for cluster config: %v", err)
	}
	rbacClient, err := rbacclientset.NewForConfig(o.clusterConfig)
	if err != nil {
		return fmt.Errorf("could not get image client for cluster config: %v", err)
	}

	if _, err := client.ServiceAccounts(o.namespace).Create(&coreapi.ServiceAccount{
		ObjectMeta: meta.ObjectMeta{
			Name: "cleanup",
		},
	}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("could not create service account for cleanup: %v", err)
	}
	if _, err := rbacClient.RoleBindings(o.namespace).Create(&rbacapi.RoleBinding{
		ObjectMeta: meta.ObjectMeta{
			Name: "cleanup",
		},
		Subjects: []rbacapi.Subject{{Kind: "ServiceAccount", Name: "cleanup"}},
		RoleRef: rbacapi.RoleRef{
			Kind: "ClusterRole",
			Name: "admin",
		},
	}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("could not create role binding for cleanup: %v", err)
	}

	grace := int64(30)
	deadline := int64(12 * time.Hour / time.Second)
	if _, err := client.Pods(o.namespace).Create(&coreapi.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "cleanup-when-idle",
		},
		Spec: coreapi.PodSpec{
			ActiveDeadlineSeconds:         &deadline,
			RestartPolicy:                 coreapi.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			ServiceAccountName:            "cleanup",
			Containers: []coreapi.Container{
				{
					Name:  "cleanup",
					Image: "openshift/origin-cli:latest",
					Env: []coreapi.EnvVar{
						{
							Name:      "NAMESPACE",
							ValueFrom: &coreapi.EnvVarSource{FieldRef: &coreapi.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
						},
						{
							Name:  "WAIT",
							Value: fmt.Sprintf("%d", int(o.idleCleanupDuration.Seconds())),
						},
					},
					Command: []string{"/bin/bash", "-c"},
					Args: []string{`
						#!/bin/bash
						set -euo pipefail

						function cleanup() {
							set +e
							oc delete project ${NAMESPACE}
						}

						trap 'kill $(jobs -p); echo "Pod deleted, deleting project ..."; exit 1' TERM
						trap cleanup EXIT

						echo "Waiting for all running pods to terminate (max idle ${WAIT}s) ..."
						count=0
						while true; do
							alive="$( oc get pods --template '{{ range .items }}{{ if and (not (eq .metadata.name "cleanup-when-idle")) (eq .spec.restartPolicy "Never") (or (eq .status.phase "Pending") (eq .status.phase "Running") (eq .status.phase "Unknown")) }} {{ .metadata.name }}{{ end }}{{ end }}' )"
							if [[ -n "${alive}" ]]; then
								count=0
								sleep ${WAIT} & wait
								continue
							fi
							if [[ "${count}" -lt 1 ]]; then
								count+=1
								sleep ${WAIT} & wait
								continue
							fi
							echo "No pods running for more than ${WAIT}s, deleting project ..."
							exit 0
						done
						`,
					},
				},
			},
		},
	}); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("could not create pod for cleanup: %v", err)
	}
	return nil
}