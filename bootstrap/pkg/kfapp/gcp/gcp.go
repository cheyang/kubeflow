/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gcp

import (
	"encoding/base64"
	"fmt"
	"github.com/cenkalti/backoff"
	"github.com/deckarep/golang-set"
	"github.com/ghodss/yaml"
	bootstrap "github.com/kubeflow/kubeflow/bootstrap/cmd/bootstrap/app"
	configtypes "github.com/kubeflow/kubeflow/bootstrap/config"
	kfapis "github.com/kubeflow/kubeflow/bootstrap/pkg/apis"
	kftypes "github.com/kubeflow/kubeflow/bootstrap/pkg/apis/apps"
	kfdefs "github.com/kubeflow/kubeflow/bootstrap/pkg/apis/apps/kfdef/v1alpha1"
	"github.com/kubeflow/kubeflow/bootstrap/pkg/utils"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gke "google.golang.org/api/container/v1"
	"google.golang.org/api/deploymentmanager/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iam/v1"
	"google.golang.org/api/serviceusage/v1"
	"io"
	"io/ioutil"
	"k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TODO: golint complains that we should not use all capital var name.
const (
	GCP_CONFIG        = "gcp_config"
	CONFIG_FILE       = "cluster-kubeflow.yaml"
	STORAGE_FILE      = "storage-kubeflow.yaml"
	NETWORK_FILE      = "network.yaml"
	GCFS_FILE         = "gcfs.yaml"
	ADMIN_SECRET_NAME = "admin-gcp-sa"
	USER_SECRET_NAME  = "user-gcp-sa"
	KUBEFLOW_OAUTH    = "kubeflow-oauth"
	IMPORTS           = "imports"
	PATH              = "path"
	CLIENT_ID         = "CLIENT_ID"
	CLIENT_SECRET     = "CLIENT_SECRET"
	BASIC_AUTH_SECRET = "kubeflow-login"
	KUBECONFIG_FORMAT = "gke_{project}_{zone}_{cluster}"
)

// The namespace for Istio
const IstioNamespace = "istio-system"

// Gcp implements KfApp Interface
// It includes the KsApp along with additional Gcp types
type Gcp struct {
	kfdefs.KfDef
	client      *http.Client
	tokenSource oauth2.TokenSource
	// When isCLI is false, following code need to be multi-thread safe, and can not access local configs or gcloud cli
	isCLI bool
	// requried when choose basic-auth
	username        string
	encodedPassword string
	// requried when choose iap
	oauthId     string
	oauthSecret string
}

// GetKfApp returns the gcp kfapp. It's called by coordinator.GetKfApp
func GetKfApp(kfdef *kfdefs.KfDef) (kftypes.KfApp, error) {
	ctx := context.Background()
	client, err := google.DefaultClient(ctx, gke.CloudPlatformScope)
	if err != nil {
		log.Fatalf("Could not authenticate Client: %v", err)
		return nil, err
	}
	ts, err := google.DefaultTokenSource(ctx, iam.CloudPlatformScope)
	if err != nil {
		return nil, &kfapis.KfError{
			Code:    int(kfapis.INVALID_ARGUMENT),
			Message: fmt.Sprintf("Get token error: %v", err),
		}
	}
	_gcp := &Gcp{
		KfDef:       *kfdef,
		client:      client,
		tokenSource: ts,
		isCLI:       true,
	}
	if _gcp.Spec.Email == "" {
		if err = _gcp.getAccount(); err != nil {
			log.Infof("cannot get gcloud account email. Error: %v", err)
		}
	}
	return _gcp, nil
}

func getSA(name string, nameSuffix string, project string) string {
	return fmt.Sprintf("%v-%v@%v.iam.gserviceaccount.com", name, nameSuffix, project)
}

// getAccount if --email is not supplied try and get account info using gcloud
func (gcp *Gcp) getAccount() error {
	output, err := exec.Command("gcloud", "config", "get-value", "account").Output()
	if err != nil {
		return fmt.Errorf("could not call 'gcloud config get-value account': %v", err)
	}
	account := string(output)
	gcp.Spec.Email = strings.TrimSpace(account)
	return nil
}

func (gcp *Gcp) writeConfigFile() error {
	buf, bufErr := yaml.Marshal(gcp.KfDef)
	if bufErr != nil {
		return bufErr
	}
	cfgFilePath := filepath.Join(gcp.Spec.AppDir, kftypes.KfConfigFile)
	cfgFilePathErr := ioutil.WriteFile(cfgFilePath, buf, 0644)
	if cfgFilePathErr != nil {
		return cfgFilePathErr
	}
	return nil
}

// Simple deploymentmanager.TargetConfiguration factory method. This method assumes imported paths
// are all within the same filesystem. From gcloud CLI source codes it appears URL is a possible
// option. We might need to update this method or find a way to work with Python source code from
// gcloud.
func generateTarget(configPath string) (*deploymentmanager.TargetConfiguration, error) {
	if !filepath.IsAbs(configPath) {
		if p, err := filepath.Abs(configPath); err != nil {
			return nil, fmt.Errorf("Getting absolute path error: %v", err)
		} else {
			configPath = p
		}
	}
	log.Infof("Reading config file: %v", configPath)
	configBuf, bufErr := ioutil.ReadFile(configPath)
	if bufErr != nil {
		return nil, fmt.Errorf("Reading config file error: %v", bufErr)
	}
	targetConfig := &deploymentmanager.TargetConfiguration{
		Config: &deploymentmanager.ConfigFile{
			Content: string(configBuf),
		},
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal(configBuf, &config); err != nil {
		return nil, fmt.Errorf("Unable to read YAML: %v", err)
	}
	if _, ok := config[IMPORTS]; !ok {
		return targetConfig, nil
	}

	entries := config[IMPORTS].([]interface{})
	dirName := filepath.Dir(configPath)
	for _, entry := range entries {
		entryMap := entry.(map[string]interface{})
		if _, ok := entryMap[PATH]; !ok {
			continue
		}
		importPath := entryMap[PATH].(string)
		if !filepath.IsAbs(importPath) {
			importPath = path.Join(dirName, importPath)
		}
		log.Infof("Reading import file: %v", importPath)
		if buf, err := ioutil.ReadFile(importPath); err == nil {
			targetConfig.Imports = append(targetConfig.Imports, &deploymentmanager.ImportFile{
				Name:    entryMap[PATH].(string),
				Content: string(buf),
			})
		} else {
			return nil, fmt.Errorf("error reading import file: %v", err)
		}
	}
	return targetConfig, nil
}

func (gcp *Gcp) getK8sClientset(ctx context.Context) (*clientset.Clientset, error) {
	cluster, err := utils.GetClusterInfo(ctx, gcp.Spec.Project,
		gcp.Spec.Zone, gcp.Name, gcp.tokenSource)
	if err != nil {
		return nil, fmt.Errorf("get Cluster error: %v", err)
	}
	config, err := utils.BuildConfigFromClusterInfo(ctx, cluster, gcp.tokenSource)
	if err != nil {
		return nil, fmt.Errorf("build ClientConfig error: %v", err)
	}

	return clientset.NewForConfig(config)
}

func blockingWait(project string, opName string, deploymentmanagerService *deploymentmanager.Service,
	ctx context.Context, logPrefix string) error {
	// Explicitly copy string to avoid memory leak.
	p := "" + project
	name := "" + opName
	return backoff.Retry(func() error {
		op, err := deploymentmanagerService.Operations.Get(p, name).Context(ctx).Do()

		if err != nil {
			// Retry here as there's a chance to get error for newly created DM operation.
			return fmt.Errorf("%v error: %v", logPrefix, err)
		}
		if op.Error != nil {
			for _, e := range op.Error.Errors {
				log.Errorf("%v error: %+v", logPrefix, e)
			}
		}
		if op.Status == "DONE" {
			if op.HttpErrorStatusCode > 0 {
				return backoff.Permanent(fmt.Errorf("%v error(%v): %v",
					logPrefix,
					op.HttpErrorStatusCode, op.HttpErrorMessage))
			}
			log.Infof("%v is finished: %v", logPrefix, op.Status)
			return nil
		}
		log.Warnf("%v status: %v (op = %v)", logPrefix, op.Status, op.Name)
		name = op.Name
		return fmt.Errorf("%v did not succeed; status: %v (op = %v)", logPrefix, op.Status, op.Name)
	}, backoff.NewExponentialBackOff())
}

func (gcp *Gcp) updateDeployment(deployment string, yamlfile string) error {
	appDir := gcp.Spec.AppDir
	gcpConfigDir := path.Join(appDir, GCP_CONFIG)
	ctx := context.Background()
	deploymentmanagerService, err := deploymentmanager.New(gcp.client)
	if err != nil {
		return fmt.Errorf("Error creating deploymentmanagerService: %v", err)
	}
	filePath := filepath.Join(gcpConfigDir, yamlfile)
	dp := &deploymentmanager.Deployment{
		Name: deployment,
	}
	if target, targetErr := generateTarget(filePath); targetErr != nil {
		return targetErr
	} else {
		dp.Target = target
	}

	project := gcp.Spec.Project
	resp, err := deploymentmanagerService.Deployments.Get(project, deployment).Context(ctx).Do()
	if err == nil {
		dp.Fingerprint = resp.Fingerprint
		opName := resp.Operation.Name
		if resp.Operation.Status == "DONE" {
			log.Infof("Updating deployment %v", deployment)
			op, updateErr := deploymentmanagerService.Deployments.Update(project, deployment, dp).Context(ctx).Do()
			if updateErr != nil {
				return fmt.Errorf("Update deployment error: %v", updateErr)
			}
			opName = op.Name
		} else {
			log.Infof("Wait running deployment %v to finish; operation name: %v.", deployment, opName)
		}
		return blockingWait(project, opName, deploymentmanagerService, ctx,
			"Updating "+deployment)
	} else {
		log.Infof("Creating deployment %v", deployment)
		op, insertErr := deploymentmanagerService.Deployments.Insert(project, dp).Context(ctx).Do()
		if insertErr != nil {
			return fmt.Errorf("Insert deployment error: %v", insertErr)
		}
		return blockingWait(project, op.Name, deploymentmanagerService, ctx,
			"Creating "+deployment)
	}
}

func createNamespace(k8sClientset *clientset.Clientset, namespace string) error {
	log.Infof("Creating namespace: %v", namespace)
	_, err := k8sClientset.CoreV1().Namespaces().Get(namespace, metav1.GetOptions{})
	if err == nil {
		log.Infof("Namespace already exists...")
		return nil
	}
	log.Infof("Get namespace error: %v", err)
	_, err = k8sClientset.CoreV1().Namespaces().Create(
		&v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		},
	)
	return err
}

func bindAdmin(k8sClientset *clientset.Clientset, user string) error {
	log.Infof("Binding admin role for %v ...", user)
	defaultAdmin := "default-admin"
	_, err := k8sClientset.RbacV1().ClusterRoleBindings().Get(defaultAdmin,
		metav1.GetOptions{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "rbac.authorization.k8s.io/v1beta1",
				Kind:       "ClusterRoleBinding",
			},
		})

	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "rbac.authorization.k8s.io/v1beta1",
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "default-admin",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: rbacv1.UserKind,
				Name: user,
			},
		},
	}
	if err == nil {
		log.Infof("Updating default-admin...")
		_, err = k8sClientset.RbacV1().ClusterRoleBindings().Update(binding)
	} else {
		log.Infof("default-admin not found, creating...")
		_, err = k8sClientset.RbacV1().ClusterRoleBindings().Create(binding)
	}
	return err
}

func (gcp *Gcp) ConfigK8s() error {
	ctx := context.Background()
	k8sClientset, err := gcp.getK8sClientset(ctx)
	if err != nil {
		return err
	}
	if err = createNamespace(k8sClientset, gcp.Namespace); err != nil {
		return fmt.Errorf("Creating namespace error: %v", err)
	}
	if err = bindAdmin(k8sClientset, gcp.Spec.Email); err != nil {
		return fmt.Errorf("Binding user as admin error: %v", err)
	}

	return nil
}

// Add a conveniently named context to KUBECONFIG.
func (gcp *Gcp) AddNamedContext() error {
	name := strings.Replace(KUBECONFIG_FORMAT, "{project}", gcp.Spec.Project, 1)
	name = strings.Replace(name, "{zone}", gcp.Spec.Zone, 1)
	name = strings.Replace(name, "{cluster}", gcp.Name, 1)
	log.Infof("KUBECONFIG name is %v", name)

	buf, err := ioutil.ReadFile(kftypes.KubeConfigPath())
	if err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Reading KUBECONFIG error: %v", err),
		}
	}
	var config map[string]interface{}
	if err = yaml.Unmarshal(buf, &config); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Unmarshaling KUBECONFIG error: %v", err),
		}
	}

	configNameChecker := func(config map[string]interface{}, entryName string, name string) error {
		e, ok := config[entryName]
		if !ok {
			return &kfapis.KfError{
				Code:    int(kfapis.INTERNAL_ERROR),
				Message: fmt.Sprintf("Not able to find %v in KUBECONFIG", entryName),
			}
		}
		entries := e.([]interface{})
		for _, entry := range entries {
			en := entry.(map[string]interface{})
			if mm, ok := en["name"]; ok {
				n := mm.(string)
				if n == name {
					return nil
				}
			} else {
				return &kfapis.KfError{
					Code:    int(kfapis.INTERNAL_ERROR),
					Message: "Not able to find name in the entry",
				}
			}
		}
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Not able to find %v from %v in KUBECONFIG", name, entryName),
		}
	}

	if err = configNameChecker(config, "clusters", name); err != nil {
		return err
	}
	if err = configNameChecker(config, "users", name); err != nil {
		return err
	}
	if err = configNameChecker(config, "contexts", name); err != nil {
		return err
	}

	e, ok := config["contexts"]
	if !ok {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: "Not able to find contexts in KUBECONFIG",
		}
	}
	contexts := e.([]interface{})
	context := make(map[string]interface{})
	context["name"] = gcp.Name
	context["context"] = map[string]string{
		"cluster":   name,
		"user":      name,
		"namespace": gcp.Namespace,
	}
	for idx, ctx := range contexts {
		c := ctx.(map[string]interface{})
		if c["name"] == gcp.Name {
			// Remove the entry to override.
			contexts = append(contexts[:idx], contexts[idx+1:]...)
			break
		}
	}
	contexts = append(contexts, context)
	config["contexts"] = contexts
	config["current-context"] = gcp.Name

	buf, err = yaml.Marshal(config)
	if err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when marshaling KUBECONFIG: %v", err),
		}
	}
	if err = ioutil.WriteFile(kftypes.KubeConfigPath(), buf, 0644); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when writing KUBECONFIG: %v", err),
		}
	}

	log.Infof("KUBECONFIG context %v is created and currently using", gcp.Name)
	return nil
}

func (gcp *Gcp) updateDM(resources kftypes.ResourceEnum) error {
	ctx := context.Background()
	gcpClient := oauth2.NewClient(ctx, gcp.tokenSource)
	if err := gcp.updateDeployment(gcp.Name+"-storage", STORAGE_FILE); err != nil {
		return fmt.Errorf("could not update %v: %v", STORAGE_FILE, err)
	}
	if err := gcp.updateDeployment(gcp.Name, CONFIG_FILE); err != nil {
		return fmt.Errorf("could not update %v: %v", CONFIG_FILE, err)
	}
	if _, networkStatErr := os.Stat(path.Join(gcp.Spec.AppDir, NETWORK_FILE)); !os.IsNotExist(networkStatErr) {
		err := gcp.updateDeployment(gcp.Name+"-network", NETWORK_FILE)
		if err != nil {
			return fmt.Errorf("could not update %v: %v", NETWORK_FILE, err)
		}
	}
	if _, gcfsStatErr := os.Stat(path.Join(gcp.Spec.AppDir, GCFS_FILE)); !os.IsNotExist(gcfsStatErr) {
		err := gcp.updateDeployment(gcp.Name+"-gcfs", GCFS_FILE)
		if err != nil {
			return fmt.Errorf("could not update %v: %v", GCFS_FILE, err)
		}
	}

	policy, policyErr := utils.GetIamPolicy(gcp.Spec.Project, gcpClient)
	if policyErr != nil {
		return fmt.Errorf("GetIamPolicy error: %v", policyErr)
	}
	appDir := gcp.Spec.AppDir
	gcpConfigDir := path.Join(appDir, GCP_CONFIG)
	iamPolicy, iamPolicyErr := utils.ReadIamBindingsYAML(
		filepath.Join(gcpConfigDir, "iam_bindings.yaml"))
	if iamPolicyErr != nil {
		return fmt.Errorf("Read IAM policy YAML error: %v", iamPolicyErr)
	}
	utils.ClearIamPolicy(policy, gcp.Name, gcp.Spec.Project)
	if err := utils.SetIamPolicy(gcp.Spec.Project, policy, gcpClient); err != nil {
		return fmt.Errorf("Set Cleared IamPolicy error: %v", err)
	}

	// Need to read policy again as latest Etag changed.
	newPolicy, policyErr := utils.GetIamPolicy(gcp.Spec.Project, gcpClient)
	if policyErr != nil {
		return fmt.Errorf("GetIamPolicy error: %v", policyErr)
	}
	utils.RewriteIamPolicy(newPolicy, iamPolicy)
	if err := utils.SetIamPolicy(gcp.Spec.Project, newPolicy, gcpClient); err != nil {
		return fmt.Errorf("Set New IamPolicy error: %v", err)
	}

	if err := gcp.ConfigK8s(); err != nil {
		return fmt.Errorf("Configure K8s is failed: %v", err)
	}

	cluster, err := utils.GetClusterInfo(ctx, gcp.Spec.Project,
		gcp.Spec.Zone, gcp.Name, gcp.tokenSource)
	if err != nil {
		return fmt.Errorf("Get Cluster error: %v", err)
	}
	client, err := utils.BuildConfigFromClusterInfo(ctx, cluster, gcp.tokenSource)
	if err != nil {
		return fmt.Errorf("Build ClientConfig error: %v", err)
	}
	// Install Istio
	if gcp.Spec.UseIstio {
		log.Infof("Installing istio...")
		parentDir := path.Dir(gcp.Spec.Repo)
		err = bootstrap.CreateResourceFromFile(client, path.Join(parentDir, "dependencies/istio/install/crds.yaml"))
		if err != nil {
			log.Errorf("Failed to create istio CRD: %v", err)
			return err
		}
		err = bootstrap.CreateResourceFromFile(client, path.Join(parentDir, "dependencies/istio/install/istio-noauth.yaml"))
		if err != nil {
			log.Errorf("Failed to create istio manifest: %v", err)
			return err
		}
		err = bootstrap.CreateResourceFromFile(client, path.Join(parentDir, "dependencies/istio/kf-istio-resources.yaml"))
		if err != nil {
			log.Errorf("Failed to create kubeflow istio resource: %v", err)
			return err
		}
		log.Infof("Done installing istio.")
	}
	return nil
}

// Apply applies the gcp kfapp.
// Remind: Need to be thread-safe: this entry is share among kfctl and deploy app
func (gcp *Gcp) Apply(resources kftypes.ResourceEnum) error {
	// kfctl only
	if gcp.isCLI {
		if gcp.Spec.UseBasicAuth {
			if os.Getenv(kftypes.KUBEFLOW_USERNAME) == "" || os.Getenv(kftypes.KUBEFLOW_PASSWORD) == "" {
				return fmt.Errorf("gcp apply needs ENV %v and %v set when using basic auth",
					kftypes.KUBEFLOW_USERNAME, kftypes.KUBEFLOW_PASSWORD)
			}
			gcp.username = os.Getenv(kftypes.KUBEFLOW_USERNAME)
			password := os.Getenv(kftypes.KUBEFLOW_PASSWORD)
			passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
			if err != nil {
				return fmt.Errorf("Error when hashing password: %v", err)
			}
			gcp.encodedPassword = base64.StdEncoding.EncodeToString(passwordHash)
		} else {
			if os.Getenv(CLIENT_ID) == "" {
				return fmt.Errorf("Need to set environment variable `%v` for IAP.",
					CLIENT_ID)
			}
			if os.Getenv(CLIENT_SECRET) == "" {
				return fmt.Errorf("Need to set environment variable `%v` for IAP.",
					CLIENT_SECRET)
			}
			gcp.oauthId = os.Getenv(CLIENT_ID)
			gcp.oauthSecret = os.Getenv(CLIENT_SECRET)
		}
	}

	// Update deployment manager
	updateDMErr := gcp.updateDM(resources)
	if updateDMErr != nil {
		return fmt.Errorf("gcp apply could not update deployment manager Error %v", updateDMErr)
	}
	// Insert secrets into the cluster
	secretsErr := gcp.createSecrets()
	if secretsErr != nil {
		return fmt.Errorf("gcp apply could not create secrets Error %v", secretsErr)
	}

	// kfctl only
	if gcp.isCLI {
		// TODO(#2604): Need to create a named context.
		cred_cmd := exec.Command("gcloud", "container", "clusters", "get-credentials",
			gcp.Name,
			"--zone="+gcp.Spec.Zone,
			"--project="+gcp.Spec.Project)
		cred_cmd.Stdout = os.Stdout
		log.Infof("Running get-credentials %v --zone=%v --project=%v ...", gcp.KfDef.Name,
			gcp.KfDef.Spec.Zone, gcp.KfDef.Spec.Project)
		if err := cred_cmd.Run(); err != nil {
			return fmt.Errorf("Error when running gcloud container clusters get-credentials: %v", err)
		}
		if _, err := os.Stat(kftypes.KubeConfigPath()); !os.IsNotExist(err) {
			gcp.AddNamedContext()
		}
	}
	return nil
}

// Try to get information for the deployment. If returned, delete it.
func deleteDeployment(deploymentmanagerService *deploymentmanager.Service, ctx context.Context,
	project string, name string) error {
	_, err := deploymentmanagerService.Deployments.Get(project, name).Context(ctx).Do()
	if err != nil {
		e := err.(*googleapi.Error)
		if e.Code == 404 {
			// Don't treat not found deployment deletion as error to make kfctl delete idempotent.
			log.Infof("Deployment %v/%v is not found during deletion.", project, name)
			return nil
		} else {
			return fmt.Errorf("Deployment %v/%v has unexpected error: %v", project, name, err)
		}
	}

	op, err := deploymentmanagerService.Deployments.Delete(project, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("Gcp.Delete is failed for %v/%v: %v", project, name, err)
	}
	if err = blockingWait(project, op.Name, deploymentmanagerService, ctx,
		"Deleting "+name); err != nil {
		return fmt.Errorf("Gcp.Delete is failed for %v/%v: %v", project, name, err)
	}
	return nil
}

func (gcp *Gcp) Delete(resources kftypes.ResourceEnum) error {
	ctx := context.Background()
	// TODO: make client a parameter
	client, err := google.DefaultClient(ctx, deploymentmanager.CloudPlatformScope)
	if err != nil {
		return fmt.Errorf("Error getting DefaultClient: %v", err)
	}
	deploymentmanagerService, err := deploymentmanager.New(client)
	if err != nil {
		return fmt.Errorf("Error creating deploymentmanagerService: %v", err)
	}

	// cluster and storage deployments are required to be deleted. network and gcfs deployments are optional.
	project := gcp.Spec.Project
	deletingDeployments := []string{
		gcp.Name,
	}
	if gcp.Spec.DeleteStorage {
		deletingDeployments = append(deletingDeployments, gcp.Name+"-storage")
	}
	if _, networkStatErr := os.Stat(path.Join(gcp.Spec.AppDir, NETWORK_FILE)); !os.IsNotExist(networkStatErr) {
		deletingDeployments = append(deletingDeployments, gcp.Name+"-network")
	}
	if _, gcfsStatErr := os.Stat(path.Join(gcp.Spec.AppDir, GCFS_FILE)); !os.IsNotExist(gcfsStatErr) {
		deletingDeployments = append(deletingDeployments, gcp.Name+"-gcfs")
	}

	for _, d := range deletingDeployments {
		if err = deleteDeployment(deploymentmanagerService, ctx, project, d); err != nil {
			return err
		}
	}

	policy, err := utils.GetIamPolicy(project, client)
	if err != nil {
		return fmt.Errorf("Error when getting IAM policy: %v", err)
	}
	saSet := mapset.NewSet(
		"serviceAccount:"+getSA(gcp.Name, "admin", project),
		"serviceAccount:"+getSA(gcp.Name, "user", project),
		"serviceAccount:"+getSA(gcp.Name, "vm", project))
	for idx, binding := range policy.Bindings {
		cleanedMembers := []string{}
		for _, member := range binding.Members {
			if saSet.Contains(member) {
				log.Infof("Removing %v from %v", member, binding.Role)
			} else {
				cleanedMembers = append(cleanedMembers, member)
			}
		}
		policy.Bindings[idx].Members = cleanedMembers
	}
	if err = utils.SetIamPolicy(project, policy, client); err != nil {
		return fmt.Errorf("Error when cleaning IAM policy: %v", err)
	}

	return nil
}

func (gcp *Gcp) copyFile(source string, dest string) error {
	from, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("cannot create directory %v", err)
	}
	defer from.Close()
	to, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return fmt.Errorf("cannot create dest file %v  Error %v", dest, err)
	}
	defer to.Close()
	_, err = io.Copy(to, from)
	if err != nil {
		return fmt.Errorf("copy failed source %v dest %v Error %v", source, dest, err)
	}

	return nil
}

// Usage: a = setNameVal(a, "acmeEmail", gcp.Spec.Email, true), similar to append
func setNameVal(entries []configtypes.NameValue, name string, val string, required bool) []configtypes.NameValue {
	for i, nv := range entries {
		if nv.Name == name {
			log.Infof("Setting %v to %v", name, val)
			entries[i].Value = val
			return entries
		}
	}
	log.Infof("Appending %v as %v", name, val)
	entries = append(entries, configtypes.NameValue{
		Name:         name,
		Value:        val,
		InitRequired: required,
	})
	return entries
}

// Helper function to generate account field for IAP.
func (gcp *Gcp) getIapAccount() string {
	iapAcct := "serviceAccount:" + gcp.Spec.Email
	if !strings.Contains(gcp.Spec.Email, "iam.gserviceaccount.com") {
		iapAcct = "user:" + gcp.Spec.Email
	}
	return iapAcct
}

// Write IAM binding rules based on GCP app config.
func (gcp *Gcp) writeIamBindingsFile(src string, dest string) error {
	buf, err := ioutil.ReadFile(src)
	if err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when reading template %v: %v", src, err),
		}
	}

	var data map[string]interface{}
	if err = yaml.Unmarshal(buf, &data); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when unmarshaling template %v: %v", src, err),
		}
	}

	e, ok := data["bindings"]
	if !ok {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: "Invalid IAM bindings format: not able to find `bindings` entry.",
		}
	}

	roles := map[string]string{
		"set-kubeflow-admin-service-account": "serviceAccount:" + getSA(gcp.Name, "admin", gcp.Spec.Project),
		"set-kubeflow-user-service-account":  "serviceAccount:" + getSA(gcp.Name, "user", gcp.Spec.Project),
		"set-kubeflow-vm-service-account":    "serviceAccount:" + getSA(gcp.Name, "vm", gcp.Spec.Project),
		"set-kubeflow-iap-account":           gcp.getIapAccount(),
	}

	bindings := e.([]interface{})
	for idx, b := range bindings {
		binding := b.(map[string]interface{})
		if mem, ok := binding["members"]; ok {
			members := mem.([]interface{})
			var newMembers []string
			for _, m := range members {
				member := m.(string)
				if acct, ok := roles[member]; ok {
					newMembers = append(newMembers, acct)
				} else {
					newMembers = append(newMembers, member)
				}
			}
			binding["members"] = newMembers
			bindings[idx] = binding
		} else {
			return &kfapis.KfError{
				Code:    int(kfapis.INTERNAL_ERROR),
				Message: "Invalid IAM bindings format: not able to find `members` entry.",
			}
		}
	}
	data["bindings"] = bindings

	if buf, err = yaml.Marshal(data); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when marshaling IAM bindings: %v", err),
		}
	}
	if err = ioutil.WriteFile(dest, buf, 0644); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when writing IAM bindings: %v", err),
		}
	}
	return nil
}

// Replace placeholders and write to cluster-kubeflow.yaml
func (gcp *Gcp) writeClusterConfig(src string, dest string) error {
	buf, err := ioutil.ReadFile(src)
	if err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when reading template %v: %v", src, err),
		}
	}

	var data map[string]interface{}
	if err = yaml.Unmarshal(buf, &data); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when unmarshaling template %v: %v", src, err),
		}
	}

	res, ok := data["resources"]
	if !ok {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: "Invalid cluster config - not able to find resources entry.",
		}
	}

	resources := res.([]interface{})
	for idx, re := range resources {
		resource := re.(map[string]interface{})
		var properties map[string]interface{}
		if props, ok := resource["properties"]; ok {
			properties = props.(map[string]interface{})
		} else {
			properties = make(map[string]interface{})
		}
		properties["gkeApiVersion"] = kftypes.DefaultGkeApiVer
		properties["zone"] = gcp.Spec.Zone
		properties["users"] = []string{
			gcp.getIapAccount(),
		}
		properties["ipName"] = gcp.Spec.IpName
		resource["properties"] = properties
		resources[idx] = resource
	}
	data["resources"] = resources

	if buf, err = yaml.Marshal(data); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when marshaling for %v: %v", dest, err),
		}
	}
	if err = ioutil.WriteFile(dest, buf, 0644); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when writing to %v: %v", dest, err),
		}
	}

	return nil
}

// Replace placeholders and write to storage-kubeflow.yaml
func (gcp *Gcp) writeStorageConfig(src string, dest string) error {
	buf, err := ioutil.ReadFile(src)
	if err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when reading storage-kubeflow template: %v", err),
		}
	}

	var data map[string]interface{}
	if err = yaml.Unmarshal(buf, &data); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when unmarshaling template %v: %v", src, err),
		}
	}

	res, ok := data["resources"]
	if !ok {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: "Invalid storage config - not able to find resources entry.",
		}
	}

	resources := res.([]interface{})
	for idx, re := range resources {
		resource := re.(map[string]interface{})
		var properties map[string]interface{}
		if props, ok := resource["properties"]; ok {
			properties = props.(map[string]interface{})
		} else {
			properties = make(map[string]interface{})
		}
		properties["zone"] = gcp.Spec.Zone
		properties["createPipelinePersistentStorage"] = true
		resource["properties"] = properties
		resources[idx] = resource
	}
	data["resources"] = resources

	if buf, err = yaml.Marshal(data); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when marshaling for %v: %v", dest, err),
		}
	}
	if err = ioutil.WriteFile(dest, buf, 0644); err != nil {
		return &kfapis.KfError{
			Code:    int(kfapis.INTERNAL_ERROR),
			Message: fmt.Sprintf("Error when writing to %v: %v", dest, err),
		}
	}

	return nil
}

func (gcp *Gcp) generateDMConfigs() error {
	appDir := gcp.Spec.AppDir
	gcpConfigDir := path.Join(appDir, GCP_CONFIG)
	gcpConfigDirErr := os.MkdirAll(gcpConfigDir, os.ModePerm)
	if gcpConfigDirErr != nil {
		return fmt.Errorf("cannot create directory %v", gcpConfigDirErr)
	}
	repo := gcp.Spec.Repo
	parentDir := path.Dir(repo)
	sourceDir := path.Join(parentDir, "deployment/gke/deployment_manager_configs")
	files := []string{"cluster.jinja", "cluster.jinja.schema", "storage.jinja",
		"storage.jinja.schema"}
	for _, file := range files {
		sourceFile := filepath.Join(sourceDir, file)
		destFile := filepath.Join(gcpConfigDir, file)
		copyErr := gcp.copyFile(sourceFile, destFile)
		if copyErr != nil {
			return fmt.Errorf("could not copy %v to %v Error %v", sourceFile, destFile, copyErr)
		}
	}

	// Reading from templates and write to gcp_config directory with content had placeholders
	// replaced.
	from := filepath.Join(sourceDir, "iam_bindings_template.yaml")
	to := filepath.Join(gcpConfigDir, "iam_bindings.yaml")
	if err := gcp.writeIamBindingsFile(from, to); err != nil {
		return err
	}
	from = filepath.Join(sourceDir, CONFIG_FILE)
	to = filepath.Join(gcpConfigDir, CONFIG_FILE)
	if err := gcp.writeClusterConfig(from, to); err != nil {
		return err
	}
	from = filepath.Join(sourceDir, STORAGE_FILE)
	to = filepath.Join(gcpConfigDir, STORAGE_FILE)
	if err := gcp.writeStorageConfig(from, to); err != nil {
		return err
	}

	return nil
}

func insertSecret(client *clientset.Clientset, secretName string, namespace string, data map[string][]byte) error {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: data,
	}
	_, err := client.CoreV1().Secrets(namespace).Create(secret)
	return err
}

// Create key for service account and write to GCP as secret.
func (gcp *Gcp) createGcpServiceAcctSecret(ctx context.Context, client *clientset.Clientset,
	email string, secretName string, namespace string) error {
	_, err := client.CoreV1().Secrets(namespace).Get(secretName, metav1.GetOptions{})
	if err == nil {
		log.Infof("Secret for %v already exists ...", secretName)
		return nil
	}

	log.Infof("Secret for %v not found, creating ...", secretName)
	oClient := oauth2.NewClient(ctx, gcp.tokenSource)
	iamService, err := iam.New(oClient)
	if err != nil {
		return fmt.Errorf("Get Oauth Client error: %v", err)
	}
	name := fmt.Sprintf("projects/%v/serviceAccounts/%v", gcp.Spec.Project,
		email)
	req := &iam.CreateServiceAccountKeyRequest{
		KeyAlgorithm:   "KEY_ALG_RSA_2048",
		PrivateKeyType: "TYPE_GOOGLE_CREDENTIALS_FILE",
	}
	saKey, err := iamService.Projects.ServiceAccounts.Keys.Create(name, req).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("Service account key creation error: %v", err)
	}
	privateKeyData, err := base64.StdEncoding.DecodeString(saKey.PrivateKeyData)
	if err != nil {
		return fmt.Errorf("PrivateKeyData decoding error: %v", err)
	}
	return insertSecret(client, secretName, namespace, map[string][]byte{
		secretName + ".json": privateKeyData,
	})
}

// User CLIENT_ID and CLIENT_SECRET from GCP to create a secret for IAP.
func (gcp *Gcp) createIapSecret(ctx context.Context, client *clientset.Clientset) error {
	oauthSecretNamespace := gcp.Namespace
	if gcp.Spec.UseIstio {
		oauthSecretNamespace = IstioNamespace
	}

	if _, err := client.CoreV1().Secrets(oauthSecretNamespace).
		Get(KUBEFLOW_OAUTH, metav1.GetOptions{}); err == nil {
		log.Infof("Secret for %v already exits ...", KUBEFLOW_OAUTH)
		return nil
	}

	return insertSecret(client, KUBEFLOW_OAUTH, oauthSecretNamespace, map[string][]byte{
		strings.ToLower(CLIENT_ID):     []byte(gcp.oauthId),
		strings.ToLower(CLIENT_SECRET): []byte(gcp.oauthSecret),
	})
}

// Use username and password provided by user and create secret for basic auth.
func (gcp *Gcp) createBasicAuthSecret(client *clientset.Clientset) error {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BASIC_AUTH_SECRET,
			Namespace: gcp.Namespace,
		},
		Data: map[string][]byte{
			"username":     []byte(gcp.username),
			"passwordhash": []byte(gcp.encodedPassword),
		},
	}
	_, err := client.CoreV1().Secrets(gcp.KfDef.Namespace).Update(secret)
	if err != nil {
		log.Warnf("Updating basic auth login is failed, trying to create one: %v", err)
		_, err = client.CoreV1().Secrets(gcp.Namespace).Create(secret)
	}
	return err
}

func (gcp *Gcp) createSecrets() error {
	ctx := context.Background()
	k8sClient, err := gcp.getK8sClientset(ctx)
	if err != nil {
		return fmt.Errorf("Get K8s clientset error: %v", err)
	}
	adminEmail := getSA(gcp.Name, "admin", gcp.Spec.Project)
	userEmail := getSA(gcp.Name, "user", gcp.Spec.Project)
	if err := gcp.createGcpServiceAcctSecret(ctx, k8sClient, adminEmail, ADMIN_SECRET_NAME, gcp.Namespace); err != nil {
		return fmt.Errorf("cannot create admin secret %v Error %v", ADMIN_SECRET_NAME, err)
	}
	if err := gcp.createGcpServiceAcctSecret(ctx, k8sClient, userEmail, USER_SECRET_NAME, gcp.Namespace); err != nil {
		return fmt.Errorf("cannot create user secret %v Error %v", USER_SECRET_NAME, err)
	}
	// Also create service account secret in istio namespace
	if gcp.Spec.UseIstio {
		if err := gcp.createGcpServiceAcctSecret(ctx, k8sClient, adminEmail, ADMIN_SECRET_NAME, IstioNamespace); err != nil {
			return fmt.Errorf("cannot create admin secret %v Error %v", ADMIN_SECRET_NAME, err)
		}
		if err := gcp.createGcpServiceAcctSecret(ctx, k8sClient, userEmail, USER_SECRET_NAME, IstioNamespace); err != nil {
			return fmt.Errorf("cannot create user secret %v Error %v", USER_SECRET_NAME, err)
		}
	}
	if gcp.Spec.UseBasicAuth {
		if err := gcp.createBasicAuthSecret(k8sClient); err != nil {
			return fmt.Errorf("cannot create basic auth login secret: %v", err)
		}
	} else {
		if err := gcp.createIapSecret(ctx, k8sClient); err != nil {
			return fmt.Errorf("cannot create IAP auth secret: %v", err)
		}
	}
	return nil
}

// Generate generates the gcp kfapp manifest.
// Remind: Need to be thread-safe: this entry is share among kfctl and deploy app
func (gcp *Gcp) Generate(resources kftypes.ResourceEnum) error {
	if gcp.Spec.Email == "" {
		if gcp.isCLI {
			return fmt.Errorf("--email not specified and cannot get gcloud value.")
		}
		return fmt.Errorf("email not specified.")
	}
	switch resources {
	case kftypes.ALL:
		gcpConfigFilesErr := gcp.generateDMConfigs()
		if gcpConfigFilesErr != nil {
			return fmt.Errorf("could not generate deployment manager configs under %v Error: %v", GCP_CONFIG, gcpConfigFilesErr)
		}
	case kftypes.PLATFORM:
		gcpConfigFilesErr := gcp.generateDMConfigs()
		if gcpConfigFilesErr != nil {
			return fmt.Errorf("could not generate deployment manager configs under %v Error: %v", GCP_CONFIG, gcpConfigFilesErr)
		}
	}
	gcp.Spec.ComponentParams["cert-manager"] = setNameVal(gcp.Spec.ComponentParams["cert-manager"], "acmeEmail", gcp.Spec.Email, true)
	if gcp.Spec.IpName == "" {
		gcp.Spec.IpName = gcp.Name + "-ip"
	}
	if gcp.Spec.Hostname == "" {
		gcp.Spec.Hostname = gcp.Name + ".endpoints." + gcp.Spec.Project + ".cloud.goog"
	}
	if gcp.Spec.UseBasicAuth {
		gcp.Spec.ComponentParams["basic-auth-ingress"] = setNameVal(gcp.Spec.ComponentParams["basic-auth-ingress"], "ipName", gcp.Spec.IpName, true)
		gcp.Spec.ComponentParams["basic-auth-ingress"] = setNameVal(gcp.Spec.ComponentParams["basic-auth-ingress"], "hostname", gcp.Spec.Hostname, true)
	} else {
		gcp.Spec.ComponentParams["iap-ingress"] = setNameVal(gcp.Spec.ComponentParams["iap-ingress"], "ipName", gcp.Spec.IpName, true)
		gcp.Spec.ComponentParams["iap-ingress"] = setNameVal(gcp.Spec.ComponentParams["iap-ingress"], "hostname", gcp.Spec.Hostname, true)
	}
	gcp.Spec.ComponentParams["pipeline"] = setNameVal(gcp.Spec.ComponentParams["pipeline"], "mysqlPd", gcp.Name+"-storage-metadata-store", false)
	gcp.Spec.ComponentParams["pipeline"] = setNameVal(gcp.Spec.ComponentParams["pipeline"], "minioPd", gcp.Name+"-storage-artifact-store", false)

	for _, comp := range gcp.Spec.Components {
		if comp == "spartakus" {
			rand.Seed(time.Now().UnixNano())
			gcp.Spec.ComponentParams["spartakus"] = setNameVal(gcp.Spec.ComponentParams["spartakus"],
				"usageId", strconv.Itoa(rand.Int()), true)
		}
	}

	if gcp.Spec.UseIstio {
		gcp.Spec.ComponentParams["iap-ingress"] = setNameVal(gcp.Spec.ComponentParams["iap-ingress"], "useIstio", "true", false)
	}

	createConfigErr := gcp.writeConfigFile()
	if createConfigErr != nil {
		return fmt.Errorf("cannot create config file app.yaml in %v", gcp.Spec.AppDir)
	}
	return nil
}

func (gcp *Gcp) gcpInitProject() error {
	ctx := context.Background()
	serviceusageService, serviceusageServiceErr := serviceusage.New(gcp.client)
	if serviceusageServiceErr != nil {
		return fmt.Errorf("could not create service usage service %v", serviceusageServiceErr)
	}

	enabledApis := []string{
		"deploymentmanager.googleapis.com",
		"servicemanagement.googleapis.com",
		"container.googleapis.com",
		"cloudresourcemanager.googleapis.com",
		"endpoints.googleapis.com",
		"file.googleapis.com",
		"ml.googleapis.com",
		"iam.googleapis.com",
		"sqladmin.googleapis.com",
	}
	for _, api := range enabledApis {
		service := fmt.Sprintf("projects/%v/services/%v", gcp.Spec.Project, api)
		_, opErr := serviceusageService.Services.Enable(service, &serviceusage.EnableServiceRequest{}).Context(ctx).Do()
		if opErr != nil {
			return fmt.Errorf("could not enable API service %v: %v", api, opErr)
		}
	}
	return nil
}

// Init initializes a gcp kfapp
func (gcp *Gcp) Init(resources kftypes.ResourceEnum) error {
	cacheDir := path.Join(gcp.Spec.AppDir, kftypes.DefaultCacheDir)
	newPath := filepath.Join(cacheDir, gcp.Spec.Version)
	swaggerFile := filepath.Join(newPath, kftypes.DefaultSwaggerFile)
	gcp.Spec.ServerVersion = "file:" + swaggerFile
	gcp.Spec.Repo = path.Join(newPath, "kubeflow")
	createConfigErr := gcp.writeConfigFile()
	if createConfigErr != nil {
		return fmt.Errorf("cannot create config file app.yaml in %v", gcp.Spec.AppDir)
	}

	if !gcp.Spec.SkipInitProject {
		log.Infof("Not skipping GCP project init, running gcpInitProject.")
		initProjectErr := gcp.gcpInitProject()
		if initProjectErr != nil {
			return fmt.Errorf("cannot init gcp project %v", initProjectErr)
		}
	}
	return nil
}
