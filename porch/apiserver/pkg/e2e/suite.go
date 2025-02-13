// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	internalpkg "github.com/GoogleContainerTools/kpt/internal/pkg"
	kptfilev1 "github.com/GoogleContainerTools/kpt/pkg/api/kptfile/v1"
	porchtest "github.com/GoogleContainerTools/kpt/pkg/test/porch"
	porchclient "github.com/GoogleContainerTools/kpt/porch/api/generated/clientset/versioned"
	porchapi "github.com/GoogleContainerTools/kpt/porch/api/porch/v1alpha1"
	configapi "github.com/GoogleContainerTools/kpt/porch/api/porchconfig/v1alpha1"
	"github.com/GoogleContainerTools/kpt/porch/repository/pkg/git"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	coreapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	aggregatorv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

const (
	// TODO: accept a flag?
	PorchTestConfigFile = "porch-test-config.yaml"
	updateGoldenFiles   = "UPDATE_GOLDEN_FILES"
)

type GitConfig struct {
	Repo      string   `json:"repo"`
	Branch    string   `json:"branch"`
	Directory string   `json:"directory"`
	Username  string   `json:"username"`
	Password  Password `json:"password"`
}

type OciConfig struct {
	Registry string `json:"registry"`
}

type Password string

func (p Password) String() string {
	return "*************"
}

type TestSuite struct {
	*testing.T
	kubeconfig *rest.Config
	client     client.Client
	clientset  porchclient.Interface

	namespace string // K8s namespace for this test run
	local     bool   // Tests running against local dev porch
}

type Initializer interface {
	Initialize(ctx context.Context)
}

var _ Initializer = &TestSuite{}

func (t *TestSuite) Initialize(ctx context.Context) {
	cfg, err := config.GetConfig()
	if err != nil {
		t.Skipf("Skipping test suite - cannot obtain k8s client config: %v", err)
	}

	t.Logf("Testing against server: %q", cfg.Host)
	cfg.UserAgent = "Porch Test"

	scheme := createClientScheme(t.T)

	if c, err := client.New(cfg, client.Options{
		Scheme: scheme,
	}); err != nil {
		t.Fatalf("Failed to initialize k8s client (%s): %v", cfg.Host, err)
	} else {
		t.client = c
		t.kubeconfig = cfg
	}

	if cs, err := porchclient.NewForConfig(cfg); err != nil {
		t.Fatalf("Failed to initialize Porch clientset: %v", err)
	} else {
		t.clientset = cs
	}

	t.local = t.IsUsingDevPorch()

	namespace := fmt.Sprintf("porch-test-%d", time.Now().UnixMicro())
	t.CreateF(ctx, &coreapi.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	})

	t.namespace = namespace
	c := t.client
	t.Cleanup(func() {
		if err := c.Delete(ctx, &coreapi.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}); err != nil {
			t.Errorf("Failed to clean up namespace %q: %v", namespace, err)
		} else {
			t.Logf("Successfully cleaned up namespace %q", namespace)
		}
	})
}

func (t *TestSuite) IsUsingDevPorch() bool {
	porch := aggregatorv1.APIService{}
	ctx := context.TODO()
	t.GetF(ctx, client.ObjectKey{
		Name: "v1alpha1.porch.kpt.dev",
	}, &porch)
	service := coreapi.Service{}
	t.GetF(ctx, client.ObjectKey{
		Namespace: porch.Spec.Service.Namespace,
		Name:      porch.Spec.Service.Name,
	}, &service)

	return service.Spec.Type == coreapi.ServiceTypeExternalName && service.Spec.ExternalName == "host.docker.internal"
}

func (t *TestSuite) CreateGitRepo() GitConfig {
	if t.IsUsingDevPorch() {
		// Create Git server on the local machine.
		return createLocalGitServer(t.T)
	} else {
		// Deploy Git server via k8s client.
		return t.createInClusterGitServer()
	}
}

type ErrorHandler func(format string, args ...interface{})

func (t *TestSuite) get(ctx context.Context, key client.ObjectKey, obj client.Object, eh ErrorHandler) {
	if err := t.client.Get(ctx, key, obj); err != nil {
		eh("failed to get resource %s %s/%s: %v", obj.GetObjectKind().GroupVersionKind(), key.Name, key.Namespace, err)
	}
}

func (c *TestSuite) list(ctx context.Context, list client.ObjectList, opts []client.ListOption, eh ErrorHandler) {
	if err := c.client.List(ctx, list, opts...); err != nil {
		eh("failed to list resources %s %+v: %v", list.GetObjectKind().GroupVersionKind(), list, err)
	}
}

func (c *TestSuite) create(ctx context.Context, obj client.Object, opts []client.CreateOption, eh ErrorHandler) {
	if err := c.client.Create(ctx, obj, opts...); err != nil {
		eh("failed to create resource %s %s/%s: %v", obj.GetObjectKind().GroupVersionKind(), obj.GetNamespace(), obj.GetName(), err)
	}
}

func (c *TestSuite) delete(ctx context.Context, obj client.Object, opts []client.DeleteOption, eh ErrorHandler) {
	if err := c.client.Delete(ctx, obj, opts...); err != nil {
		eh("failed to delete resource %s %s/%s: %v", obj.GetObjectKind().GroupVersionKind(), obj.GetNamespace(), obj.GetName(), err)
	}
}

func (t *TestSuite) update(ctx context.Context, obj client.Object, opts []client.UpdateOption, eh ErrorHandler) {
	if err := t.client.Update(ctx, obj, opts...); err != nil {
		eh("failed to update resource %s %s/%s: %v", obj.GetObjectKind().GroupVersionKind(), obj.GetNamespace(), obj.GetName(), err)
	}
}

func (t *TestSuite) patch(ctx context.Context, obj client.Object, patch client.Patch, opts []client.PatchOption, eh ErrorHandler) {
	if err := t.client.Patch(ctx, obj, patch, opts...); err != nil {
		eh("failed to patch resource %s %s/%s: %v", obj.GetObjectKind().GroupVersionKind(), obj.GetNamespace(), obj.GetName(), err)
	}
}

func (t *TestSuite) updateApproval(ctx context.Context, obj *porchapi.PackageRevision, opts metav1.UpdateOptions, eh ErrorHandler) *porchapi.PackageRevision {
	if res, err := t.clientset.PorchV1alpha1().PackageRevisions(obj.Namespace).UpdateApproval(ctx, obj.Name, obj, opts); err != nil {
		eh("failed to update approval of %s/%s: %v", obj.Namespace, obj.Name, err)
		return nil
	} else {
		return res
	}
}

// deleteAllOf(ctx context.Context, obj Object, opts ...DeleteAllOfOption) error

func (t *TestSuite) GetE(ctx context.Context, key client.ObjectKey, obj client.Object) {
	t.get(ctx, key, obj, ErrorHandler(t.Errorf))
}

func (t *TestSuite) GetF(ctx context.Context, key client.ObjectKey, obj client.Object) {
	t.get(ctx, key, obj, ErrorHandler(t.Fatalf))
}

func (t *TestSuite) ListE(ctx context.Context, list client.ObjectList, opts ...client.ListOption) {
	t.list(ctx, list, opts, t.Errorf)
}

func (t *TestSuite) CreateF(ctx context.Context, obj client.Object, opts ...client.CreateOption) {
	t.create(ctx, obj, opts, t.Fatalf)
}

func (t *TestSuite) CreateE(ctx context.Context, obj client.Object, opts ...client.CreateOption) {
	t.create(ctx, obj, opts, t.Errorf)
}

func (t *TestSuite) DeleteF(ctx context.Context, obj client.Object, opts ...client.DeleteOption) {
	t.delete(ctx, obj, opts, t.Fatalf)
}

func (t *TestSuite) DeleteE(ctx context.Context, obj client.Object, opts ...client.DeleteOption) {
	t.delete(ctx, obj, opts, t.Errorf)
}

func (t *TestSuite) DeleteL(ctx context.Context, obj client.Object, opts ...client.DeleteOption) {
	t.delete(ctx, obj, opts, t.Logf)
}

func (t *TestSuite) UpdateF(ctx context.Context, obj client.Object, opts ...client.UpdateOption) {
	t.update(ctx, obj, opts, t.Fatalf)
}

func (t *TestSuite) UpdateE(ctx context.Context, obj client.Object, opts ...client.UpdateOption) {
	t.update(ctx, obj, opts, t.Errorf)
}

func (t *TestSuite) PatchF(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) {
	t.patch(ctx, obj, patch, opts, t.Fatalf)
}

func (t *TestSuite) PatchE(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) {
	t.patch(ctx, obj, patch, opts, t.Errorf)
}

func (t *TestSuite) UpdateApprovalF(ctx context.Context, pr *porchapi.PackageRevision, opts metav1.UpdateOptions) *porchapi.PackageRevision {
	return t.updateApproval(ctx, pr, opts, t.Fatalf)
}

// DeleteAllOf(ctx context.Context, obj Object, opts ...DeleteAllOfOption) error

func createClientScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()

	for _, api := range (runtime.SchemeBuilder{
		porchapi.AddToScheme,
		configapi.AddToScheme,
		coreapi.AddToScheme,
		aggregatorv1.AddToScheme,
		appsv1.AddToScheme,
	}) {
		if err := api(scheme); err != nil {
			t.Fatalf("Failed to initialize test k8s api client")
		}
	}
	return scheme
}

func createLocalGitServer(t *testing.T) GitConfig {
	tmp, err := os.MkdirTemp("", "porch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp directory for Git repository: %v", err)
		return GitConfig{}
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(tmp); err != nil {
			t.Errorf("Failed to delete Git temp directory %q: %v", tmp, err)
		}
	})

	isBare := true
	repo, err := gogit.PlainInit(tmp, isBare)
	if err != nil {
		t.Fatalf("Failed to initialize Git repository in %q: %v", tmp, err)
		return GitConfig{}
	}

	createInitialCommit(t, repo)

	server, err := git.NewGitServer(repo)
	if err != nil {
		t.Fatalf("Failed to start git server: %v", err)
		return GitConfig{}
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	addressChannel := make(chan net.Addr)

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := server.ListenAndServe(ctx, "127.0.0.1:0", addressChannel)
		if err != nil {
			if err == http.ErrServerClosed {
				t.Log("Git server shut down successfully")
			} else {
				t.Errorf("Git server exited with error: %v", err)
			}
		}
	}()

	// Wait for server to start up
	address, ok := <-addressChannel
	if !ok {
		t.Errorf("Server failed to start")
		return GitConfig{}
	}

	return GitConfig{
		Repo:      fmt.Sprintf("http://%s", address),
		Branch:    "main",
		Directory: "/",
	}
}

func createInitialCommit(t *testing.T, repo *gogit.Repository) {
	store := repo.Storer
	// Create first commit using empty tree.
	emptyTree := object.Tree{}
	encodedTree := store.NewEncodedObject()
	if err := emptyTree.Encode(encodedTree); err != nil {
		t.Fatalf("Failed to encode initial empty commit tree: %v", err)
	}

	treeHash, err := store.SetEncodedObject(encodedTree)
	if err != nil {
		t.Fatalf("Failed to create initial empty commit tree: %v", err)
	}

	sig := object.Signature{
		Name:  "Porch Test",
		Email: "porch-test@kpt.dev",
		When:  time.Now(),
	}

	commit := object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      "Empty Commit",
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{}, // No parents
	}

	encodedCommit := store.NewEncodedObject()
	if err := commit.Encode(encodedCommit); err != nil {
		t.Fatalf("Failed to encode initial empty commit: %v", err)
	}

	commitHash, err := store.SetEncodedObject(encodedCommit)
	if err != nil {
		t.Fatalf("Failed to create initial empty commit: %v", err)
	}

	head := plumbing.NewHashReference(plumbing.ReferenceName("refs/heads/main"), commitHash)
	if err := repo.Storer.SetReference(head); err != nil {
		t.Fatalf("Failed to set refs/heads/main to commit sha %s", commitHash)
	}
}

func (t *TestSuite) createInClusterGitServer() GitConfig {
	ctx := context.TODO()

	// Determine git-server image name. Use the same container registry and tag as the Porch server,
	// replacing base image name with `git-server`. TODO: Make configurable?

	var porch appsv1.Deployment
	t.GetF(ctx, client.ObjectKey{
		Namespace: "porch-system",
		Name:      "porch-server",
	}, &porch)

	gitImage := porchtest.InferGitServerImage(porch.Spec.Template.Spec.Containers[0].Image)

	var replicas int32 = 1
	var selector = strings.ReplaceAll(t.Name(), "/", "_")

	t.CreateF(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "git-server",
			Namespace: t.namespace,
			Annotations: map[string]string{
				"kpt.dev/porch-test": t.Name(),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"git-server": selector,
				},
			},
			Template: coreapi.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"git-server": selector,
					},
				},
				Spec: coreapi.PodSpec{
					Containers: []coreapi.Container{
						{
							Name:  "git-server",
							Image: gitImage,
							Args:  []string{},
							Ports: []coreapi.ContainerPort{
								{
									ContainerPort: 8080,
									Protocol:      coreapi.ProtocolTCP,
								},
							},
							ImagePullPolicy: coreapi.PullIfNotPresent,
						},
					},
				},
			},
		},
	})

	t.Cleanup(func() {
		t.DeleteE(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "git-server",
				Namespace: t.namespace,
			},
		})
	})

	t.CreateF(ctx, &coreapi.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "git-server-service",
			Namespace: t.namespace,
			Annotations: map[string]string{
				"kpt.dev/porch-test": t.Name(),
			},
		},
		Spec: coreapi.ServiceSpec{
			Ports: []coreapi.ServicePort{
				{
					Protocol: coreapi.ProtocolTCP,
					Port:     8080,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 8080,
					},
				},
			},
			Selector: map[string]string{
				"git-server": selector,
			},
		},
	})

	t.Cleanup(func() {
		t.DeleteE(ctx, &coreapi.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "git-server-service",
				Namespace: t.namespace,
			},
		})
	})

	t.Logf("Waiting for git-server to start ...")

	// Wait a minute for git server to start up.
	giveUp := time.Now().Add(time.Minute)

	for {
		time.Sleep(5 * time.Second)

		var server appsv1.Deployment
		t.GetF(ctx, client.ObjectKey{
			Namespace: t.namespace,
			Name:      "git-server",
		}, &server)
		if server.Status.AvailableReplicas > 0 {
			t.Logf("git server is up")
			break
		}

		if time.Now().After(giveUp) {
			t.Fatalf("git server failed to start: %s", &server)
			return GitConfig{}
		}
	}

	t.Logf("Waiting for git-server-service to be ready ...")

	// Check the Endpoint resource for readiness
	giveUp = time.Now().Add(time.Minute)

	for {
		time.Sleep(5 * time.Second)

		var endpoint coreapi.Endpoints
		err := t.client.Get(ctx, client.ObjectKey{
			Namespace: t.namespace,
			Name:      "git-server-service",
		}, &endpoint)

		if err == nil && endpointIsReady(&endpoint) {
			t.Logf("git-server-service is ready")
			break
		}

		if time.Now().After(giveUp) {
			t.Fatalf("git-server-service not ready on time: %s", &endpoint)
			return GitConfig{}
		}
	}

	return GitConfig{
		Repo:      fmt.Sprintf("http://git-server-service.%s.svc.cluster.local:8080", t.namespace),
		Branch:    "main",
		Directory: "/",
	}
}

func endpointIsReady(endpoints *coreapi.Endpoints) bool {
	if len(endpoints.Subsets) == 0 {
		return false
	}
	for _, s := range endpoints.Subsets {
		if len(s.Addresses) == 0 {
			return false
		}
		for _, a := range s.Addresses {
			if a.IP == "" {
				return false
			}
		}
	}
	return true
}

func (t *TestSuite) ParseKptfileF(resources *porchapi.PackageRevisionResources) *kptfilev1.KptFile {
	contents, ok := resources.Spec.Resources[kptfilev1.KptFileName]
	if !ok {
		t.Fatalf("Kptfile not found in %s/%s package", resources.Namespace, resources.Name)
	}
	kptfile, err := internalpkg.DecodeKptfile(strings.NewReader(contents))
	if err != nil {
		t.Fatalf("Cannot decode Kptfile (%s): %v", contents, err)
	}
	return kptfile
}

func (t *TestSuite) SaveKptfileF(resources *porchapi.PackageRevisionResources, kptfile *kptfilev1.KptFile) {
	b, err := yaml.MarshalWithOptions(kptfile, &yaml.EncoderOptions{SeqIndent: yaml.WideSequenceStyle})
	if err != nil {
		t.Fatalf("Failed saving Kptfile: %v", err)
	}
	resources.Spec.Resources[kptfilev1.KptFileName] = string(b)
}

func (t *TestSuite) FindAndDecodeF(resources *porchapi.PackageRevisionResources, name string, value interface{}) {
	contents, ok := resources.Spec.Resources[name]
	if !ok {
		t.Fatalf("Cannot find %q in %s/%s package", name, resources.Namespace, resources.Name)
	}
	var temp interface{}
	d := yaml.NewDecoder(strings.NewReader(contents))
	if err := d.Decode(&temp); err != nil {
		t.Fatalf("Cannot decode yaml %q in %s/%s package: %v", name, resources.Namespace, resources.Name, err)
	}
	jsonData, err := json.Marshal(temp)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	if err := json.Unmarshal(jsonData, value); err != nil {
		t.Fatalf("json.Unmarshal failed; %v", err)
	}
}

func (t *TestSuite) CompareGoldenFileYAML(goldenPath string, gotContents string) string {
	gotContents = normalizeYamlOrdering(t.T, gotContents)

	if os.Getenv(updateGoldenFiles) != "" {
		if err := ioutil.WriteFile(goldenPath, []byte(gotContents), 0644); err != nil {
			t.Fatalf("Failed to update golden file %q: %v", goldenPath, err)
		}
	}
	golden, err := ioutil.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("Failed to read golden file %q: %v", goldenPath, err)
	}
	return cmp.Diff(string(golden), gotContents)
}

func normalizeYamlOrdering(t *testing.T, contents string) string {
	var data interface{}
	if err := yaml.Unmarshal([]byte(contents), &data); err != nil {
		// not yaml.
		t.Fatalf("Failed to unmarshal yaml: %v\n%s\n", err, contents)
	}

	var stable bytes.Buffer
	encoder := yaml.NewEncoder(&stable)
	encoder.SetIndent(2)
	if err := encoder.Encode(data); err != nil {
		t.Fatalf("Failed to re-encode yaml output: %v", err)
	}
	return stable.String()
}
