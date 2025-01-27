package registry

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/operator-framework/operator-marketplace/pkg/apis/operators/v1"
	"github.com/operator-framework/operator-marketplace/pkg/apis/operators/v2"
	"github.com/operator-framework/operator-marketplace/pkg/builders"
	wrapper "github.com/operator-framework/operator-marketplace/pkg/client"
	"github.com/operator-framework/operator-marketplace/pkg/datastore"
	"github.com/sirupsen/logrus"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	containerName              = "registry-server"
	clusterRoleName            = "marketplace-operator-registry-server"
	portNumber                 = 50051
	portName                   = "grpc"
	deploymentUpdateAnnotation = "openshift-marketplace-update-hash"
)

var action = []string{"grpc_health_probe", "-addr=localhost:50051"}

// DefaultServerImage is the registry image to be used in the absence of
// the command line parameter.
const DefaultServerImage = "quay.io/openshift/origin-operator-registry"

// ServerImage is the image used for creating the operator registry pod.
// This gets set in the cmd/manager/main.go.
var ServerImage string

type registry struct {
	log      *logrus.Entry
	client   wrapper.Client
	reader   datastore.Reader
	source   string
	packages string
	key      types.NamespacedName
	owner    string
	image    string
	address  string
}

// Registry contains the method that ensures a registry-pod deployment and its
// associated resources are created.
type Registry interface {
	Ensure() error
	GetAddress() string
}

// NewRegistry returns an initialized instance of Registry
func NewRegistry(log *logrus.Entry, client wrapper.Client, reader datastore.Reader, key types.NamespacedName, source, packages, image, owner string) Registry {
	return &registry{
		log:      log,
		client:   client,
		reader:   reader,
		source:   source,
		packages: packages,
		key:      key,
		owner:    owner,
		image:    image,
	}
}

// Ensure ensures a registry-pod deployment and its associated
// resources are created.
func (r *registry) Ensure() error {
	appRegistries, secretIsPresent := r.getAppRegistries()

	// We create a ServiceAccount, Role and RoleBindings only if the registry
	// pod needs to access private registry which requires access to a secret
	if secretIsPresent {
		if err := r.ensureServiceAccount(); err != nil {
			return err
		}
		if err := r.ensureRole(); err != nil {
			return err
		}
		if err := r.ensureRoleBinding(); err != nil {
			return err
		}
	}

	if err := r.ensureDeployment(appRegistries, secretIsPresent); err != nil {
		return err
	}
	if err := r.ensureService(); err != nil {
		return err
	}
	return nil
}

func (r *registry) GetAddress() string {
	return r.address
}

// ensureDeployment ensures that registry Deployment is present for serving
// the the grpc interface for the packages from the given app registries.
// needServiceAccount indicates that the deployment is for a private registry
// and the pod requires a Service Account with the Role that allows it to access
// secrets.
func (r *registry) ensureDeployment(appRegistries []string, needServiceAccount bool) error {
	registryCommand := getCommand(r.packages, appRegistries)
	deployment := new(builders.DeploymentBuilder).WithTypeMeta().Deployment()
	if err := r.client.Get(context.TODO(), r.key, deployment); err != nil {
		deployment = r.newDeployment(registryCommand, needServiceAccount)
		err = r.client.Create(context.TODO(), deployment)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create Deployment %s: %v", deployment.GetName(), err)
			return err
		}
		r.log.Infof("Created Deployment %s with registry command: %s", deployment.GetName(), registryCommand)
	} else {
		// Check if the list of containers is empty. Based on that we will either create a spec
		// from scratch or update the existing container spec.
		if len(deployment.Spec.Template.Spec.Containers) == 0 {
			deployment.Spec.Template = r.newPodTemplateSpec(registryCommand, needServiceAccount)
		} else {
			// Update the command passed to the registry to account for packages being added and removed
			// from Quay
			deployment.Spec.Template.Spec.Containers[0].Command = registryCommand

			// It is possible that private app-registries were added to an OperatorSource requiring addition
			// of an authorization token. In that scenario we have to add the service account to the spec.
			// There is no harm updating in other cases.
			if needServiceAccount {
				deployment.Spec.Template.Spec.ServiceAccountName = r.key.Name
			}
		}
		// Set or update the annotation to force an update. This is required so that we get updates
		// from Quay during the sync cycle when packages have not been added or removed from the spec.
		meta.SetMetaDataAnnotation(&deployment.Spec.Template.ObjectMeta, deploymentUpdateAnnotation,
			fmt.Sprintf("%x", time.Now().UnixNano()))
		if err = r.client.Update(context.TODO(), deployment); err != nil {
			r.log.Errorf("Failed to update Deployment %s : %v", deployment.GetName(), err)
			return err
		}
		r.log.Infof("Updated Deployment %s with registry command: %s", deployment.GetName(), registryCommand)
	}
	return nil
}

// ensureRole ensure that the Role required to access secrets from the registry
// Deployment is present.
func (r *registry) ensureRole() error {
	role := new(builders.RoleBuilder).WithTypeMeta().Role()
	if err := r.client.Get(context.TODO(), r.key, role); err != nil {
		role = r.newRole()
		err = r.client.Create(context.TODO(), role)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create Role %s: %v", role.GetName(), err)
			return err
		}
		r.log.Infof("Created Role %s", role.GetName())
	} else {
		// Update the Rules to be on the safe side
		role.Rules = getRules()
		err = r.client.Update(context.TODO(), role)
		if err != nil {
			r.log.Errorf("Failed to update Role %s : %v", role.GetName(), err)
			return err
		}
		r.log.Infof("Updated Role %s", role.GetName())
	}
	return nil
}

// ensureRoleBinding ensures that the RoleBinding bound to the Role previously
// created is present.
func (r *registry) ensureRoleBinding() error {
	roleBinding := new(builders.RoleBindingBuilder).WithTypeMeta().RoleBinding()
	if err := r.client.Get(context.TODO(), r.key, roleBinding); err != nil {
		roleBinding = r.newRoleBinding(r.key.Name)
		err = r.client.Create(context.TODO(), roleBinding)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create RoleBinding %s: %v", roleBinding.GetName(), err)
			return err
		}
		r.log.Infof("Created RoleBinding %s", roleBinding.GetName())
	} else {
		// Update the Rules to be on the safe side
		roleBinding.RoleRef = builders.NewRoleRef(r.key.Name)
		err = r.client.Update(context.TODO(), roleBinding)
		if err != nil {
			r.log.Errorf("Failed to update RoleBinding %s : %v", roleBinding.GetName(), err)
			return err
		}
		r.log.Infof("Updated RoleBinding %s", roleBinding.GetName())
	}
	return nil
}

// ensureService ensure that the Service for the registry deployment is present.
func (r *registry) ensureService() error {
	service := new(builders.ServiceBuilder).WithTypeMeta().Service()
	// Delete the Service so that we get a new ClusterIP
	if err := r.client.Get(context.TODO(), r.key, service); err == nil {
		r.log.Infof("Service %s is present", service.GetName())
		err := r.client.Delete(context.TODO(), service)
		if err != nil {
			r.log.Errorf("Failed to delete Service %s", service.GetName())
			// Make a best effort to create the service
		} else {
			r.log.Infof("Deleted Service %s", service.GetName())
		}
	}
	service = r.newService()
	if err := r.client.Create(context.TODO(), service); err != nil && !errors.IsAlreadyExists(err) {
		r.log.Errorf("Failed to create Service %s: %v", service.GetName(), err)
		return err
	}
	r.log.Infof("Created Service %s", service.GetName())

	r.address = service.Spec.ClusterIP + ":" + strconv.Itoa(int(service.Spec.Ports[0].Port))
	return nil
}

// ensureServiceAccount ensure that the ServiceAccount required to be associated
// with the Deployment is present.
func (r *registry) ensureServiceAccount() error {
	serviceAccount := new(builders.ServiceAccountBuilder).WithTypeMeta().ServiceAccount()
	if err := r.client.Get(context.TODO(), r.key, serviceAccount); err != nil {
		serviceAccount = r.newServiceAccount()
		err = r.client.Create(context.TODO(), serviceAccount)
		if err != nil && !errors.IsAlreadyExists(err) {
			r.log.Errorf("Failed to create ServiceAccount %s: %v", serviceAccount.GetName(), err)
			return err
		}
		r.log.Infof("Created ServiceAccount %s", serviceAccount.GetName())
	} else {
		r.log.Infof("ServiceAccount %s is present", serviceAccount.GetName())
	}
	return nil
}

// getLabels returns the label that must match between the Deployment's
// LabelSelector and the Pod template's label
func (r *registry) getLabel() map[string]string {
	return map[string]string{"marketplace.catalogSourceConfig": r.key.Name}
}

// getAppRegistries returns a list of app registries in the format
// {base url with cnr prefix}|{app registry namespace}|{secret namespace/secret name}.
// |<secret namespace/secret name} will be present only for private repositories,
// in which case secretIsPresent will be true.
func (r *registry) getAppRegistries() (appRegistries []string, secretIsPresent bool) {
	packageIDs := v2.GetValidPackageSliceFromString(r.packages)
	for _, packageID := range packageIDs {
		opsrcMeta, err := r.reader.Read(r.source, packageID)
		if err != nil {
			r.log.Errorf("Error %v reading package %s", err, packageID)
			continue
		}
		// {base url with cnr prefix}|{app registry namespace}
		appRegistry := opsrcMeta.Endpoint + "|" + opsrcMeta.RegistryNamespace
		if opsrcMeta.SecretNamespacedName != "" {
			// {base url with cnr prefix}|{app registry namespace}|{secret namespace/secret name}
			appRegistry += "|" + opsrcMeta.SecretNamespacedName
			secretIsPresent = true
		}
		found := false
		for _, r := range appRegistries {
			if r == appRegistry {
				found = true
				break
			}
		}
		if !found {
			appRegistries = append(appRegistries, appRegistry)
		}
	}
	return
}

// getSubjects returns the Subjects that the RoleBinding should apply to.
func (r *registry) getSubjects() []rbac.Subject {
	return []rbac.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      r.key.Name,
			Namespace: r.key.Namespace,
		},
	}
}

// newDeployment() returns a Deployment object that can be used to bring up a
// registry deployment
func (r *registry) newDeployment(registryCommand []string, needServiceAccount bool) *apps.Deployment {
	builder := new(builders.DeploymentBuilder).
		WithMeta(r.key.Name, r.key.Namespace).
		WithSpec(1, r.getLabel(), r.newPodTemplateSpec(registryCommand, needServiceAccount))
	if r.owner == v1.OperatorSourceKind {
		builder.WithOpsrcOwnerLabel(r.key.Name, r.key.Namespace)
	} else if r.owner == v2.CatalogSourceConfigKind {
		builder.WithCscOwnerLabel(r.key.Name, r.key.Namespace)
	}
	return builder.Deployment()
}

// newPodTemplateSpec returns a PodTemplateSpec object that can be used to bring
// up a registry pod
func (r *registry) newPodTemplateSpec(registryCommand []string, needServiceAccount bool) core.PodTemplateSpec {
	podTemplateSpec := core.PodTemplateSpec{
		ObjectMeta: meta.ObjectMeta{
			Name:      r.key.Name,
			Namespace: r.key.Namespace,
			Labels:    r.getLabel(),
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{
					Name:    r.key.Name,
					Image:   r.image,
					Command: registryCommand,
					Ports: []core.ContainerPort{
						{
							Name:          portName,
							ContainerPort: portNumber,
						},
					},
					ReadinessProbe: &core.Probe{
						Handler: core.Handler{
							Exec: &core.ExecAction{
								Command: action,
							},
						},
						InitialDelaySeconds: 5,
						FailureThreshold:    30,
					},
					LivenessProbe: &core.Probe{
						Handler: core.Handler{
							Exec: &core.ExecAction{
								Command: action,
							},
						},
						InitialDelaySeconds: 5,
						FailureThreshold:    30,
					},
				},
			},
		},
	}
	if needServiceAccount {
		podTemplateSpec.Spec.ServiceAccountName = r.key.Name
	}
	return podTemplateSpec
}

// newRole returns a Role object with the rules set to access secrets from the
// registry pod
func (r *registry) newRole() *rbac.Role {
	builder := new(builders.RoleBuilder).
		WithMeta(r.key.Name, r.key.Namespace).
		WithRules(getRules())
	if r.owner == v1.OperatorSourceKind {
		builder.WithOpsrcOwnerLabel(r.key.Name, r.key.Namespace)
	} else if r.owner == v2.CatalogSourceConfigKind {
		builder.WithCscOwnerLabel(r.key.Name, r.key.Namespace)
	}
	return builder.Role()
}

// newRoleBinding returns a RoleBinding object RoleRef set to the given Role.
func (r *registry) newRoleBinding(roleName string) *rbac.RoleBinding {
	builder := new(builders.RoleBindingBuilder).
		WithMeta(r.key.Name, r.key.Namespace).
		WithSubjects(r.getSubjects()).
		WithRoleRef(roleName)
	if r.owner == v1.OperatorSourceKind {
		builder.WithOpsrcOwnerLabel(r.key.Name, r.key.Namespace)
	} else if r.owner == v2.CatalogSourceConfigKind {
		builder.WithCscOwnerLabel(r.key.Name, r.key.Namespace)
	}
	return builder.RoleBinding()
}

// newService returns a new Service object.
func (r *registry) newService() *core.Service {
	builder := new(builders.ServiceBuilder).
		WithMeta(r.key.Name, r.key.Namespace).
		WithSpec(r.newServiceSpec())
	if r.owner == v1.OperatorSourceKind {
		builder.WithOpsrcOwnerLabel(r.key.Name, r.key.Namespace)
	} else if r.owner == v2.CatalogSourceConfigKind {
		builder.WithCscOwnerLabel(r.key.Name, r.key.Namespace)
	}
	return builder.Service()
}

// newServiceAccount returns a new ServiceAccount object.
func (r *registry) newServiceAccount() *core.ServiceAccount {
	builder := new(builders.ServiceAccountBuilder).
		WithMeta(r.key.Name, r.key.Namespace)
	if r.owner == v1.OperatorSourceKind {
		builder.WithOpsrcOwnerLabel(r.key.Name, r.key.Namespace)
	} else if r.owner == v2.CatalogSourceConfigKind {
		builder.WithCscOwnerLabel(r.key.Name, r.key.Namespace)
	}
	return builder.ServiceAccount()
}

// newServiceSpec returns a ServiceSpec as required to front the registry deployment
func (r *registry) newServiceSpec() core.ServiceSpec {
	return core.ServiceSpec{
		Ports: []core.ServicePort{
			{
				Name:       portName,
				Port:       portNumber,
				TargetPort: intstr.FromInt(portNumber),
			},
		},
		Selector: r.getLabel(),
	}
}

// waitForDeploymentScaleDown waits for the deployment to scale down to zero within the timeout duration.
func (r *registry) waitForDeploymentScaleDown(retryInterval, timeout time.Duration) (*apps.Deployment, error) {
	deployment := apps.Deployment{}
	err := wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		err = r.client.Get(context.TODO(), r.key, &deployment)
		if err != nil {
			r.log.Errorf("Deployment %s not found: %v", deployment.GetName(), err)
			return false, err
		}

		if deployment.Status.AvailableReplicas == 0 {
			return true, nil
		}
		r.log.Infof("Waiting for scale down of Deployment %s (%d/0)\n",
			deployment.GetName(), deployment.Status.AvailableReplicas)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	r.log.Infof("Deployment %s has scaled down (%d/%d)",
		deployment.GetName(), deployment.Status.AvailableReplicas, *deployment.Spec.Replicas)
	return &deployment, nil
}

// getCommand returns the command used to launch the registry server
// Example: appregistry-server \
//    -r {base url with cnr prefix}|{app registry namespace} \
//    -r {base url with cnr prefix}|{app registry namespace}|{secret namespace/secret name} \
//    -o {packages}"
func getCommand(packages string, appRegistries []string) []string {
	command := []string{"appregistry-server"}
	for _, registry := range appRegistries {
		command = append(command, "-r", registry)
	}
	command = append(command, "-o", packages)
	return command
}

// getRules returns the PolicyRule needed to access secrets from the registry pod
func getRules() []rbac.PolicyRule {
	return []rbac.PolicyRule{
		builders.NewRule([]string{"get"}, []string{""}, []string{"secrets"}, nil),
	}
}
