/*

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

package controllers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"ariga.io/atlas/sql/migrate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	dbv1alpha1 "github.com/ariga/atlas-operator/api/v1alpha1"
	"github.com/ariga/atlas-operator/controllers/watch"
	"github.com/ariga/atlas-operator/internal/atlas"
)

// CLI is the interface used to interact with Atlas CLI
type MigrateCLI interface {
	Apply(ctx context.Context, data *atlas.ApplyParams) (*atlas.ApplyReport, error)
	Status(ctx context.Context, data *atlas.StatusParams) (*atlas.StatusReport, error)
}

// AtlasMigrationReconciler reconciles a AtlasMigration object
type AtlasMigrationReconciler struct {
	client.Client
	CLI              MigrateCLI
	Scheme           *runtime.Scheme
	secretWatcher    *watch.ResourceWatcher
	configMapWatcher *watch.ResourceWatcher
	recorder         record.EventRecorder
}

func NewAtlasMigrationReconciler(mgr manager.Manager, cli MigrateCLI) *AtlasMigrationReconciler {
	secretWatcher := watch.New()
	configMapWatcher := watch.New()
	return &AtlasMigrationReconciler{
		CLI:              cli,
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		configMapWatcher: &configMapWatcher,
		secretWatcher:    &secretWatcher,
		recorder:         mgr.GetEventRecorderFor("atlasmigration-controller"),
	}
}

// atlasMigrationData is the data used to render the HCL template
// that will be used for Atlas CLI
type (
	atlasMigrationData struct {
		EnvName         string
		URL             string
		Migration       *migration
		Cloud           *cloud
		RevisionsSchema string
	}

	migration struct {
		Dir string
	}

	cloud struct {
		URL       string
		Token     string
		Project   string
		RemoteDir *remoteDir
	}

	remoteDir struct {
		Name string
		Tag  string
	}
)

//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.atlasgo.io,resources=atlasmigrations/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AtlasMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	var am dbv1alpha1.AtlasMigration
	if err := r.Get(ctx, req.NamespacedName, &am); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// At the end of reconcile, update the status of the resource base on the error
	defer func() {
		clientErr := r.Status().Update(ctx, &am)
		if clientErr != nil {
			log.Error(clientErr, "failed to update resource status")
		}

		// After updating the status, watch the dependent resources
		r.watch(am)
	}()

	// When the resource is first created, create the "Ready" condition.
	if len(am.Status.Conditions) == 0 {
		am.SetNotReady("Reconciling", "Reconciling")
		return ctrl.Result{Requeue: true}, nil
	}

	// Extract migration data from the given resource
	md, cleanUp, err := r.extractMigrationData(ctx, am)
	if err != nil {
		am.SetNotReady("ReadingMigrationData", err.Error())
		r.recordErrEvent(am, err)
		return result(err)
	}
	defer cleanUp()
	hash, err := md.hash()
	if err != nil {
		am.SetNotReady("CalculatingHash", err.Error())
		return ctrl.Result{}, nil
	}

	// If the migration resource has changed and the resource ready condition is not false, immediately set it to false.
	// This is done so that the observed status of the migration reflects its "in-progress" state while it is being
	// reconciled.
	if am.IsReady() && am.IsHashModified(hash) {
		am.SetNotReady("Reconciling", "Current migration data has changed")
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile given resource
	status, err := r.reconcile(ctx, md)
	if err != nil {
		am.SetNotReady("Migrating", strings.TrimSpace(err.Error()))
		r.recordErrEvent(am, err)
		return result(err)
	}
	r.recorder.Eventf(&am, corev1.EventTypeNormal, "Applied", "Version %s applied", status.LastAppliedVersion)
	am.SetReady(status)
	return ctrl.Result{}, nil
}

func (r *AtlasMigrationReconciler) recordErrEvent(am dbv1alpha1.AtlasMigration, err error) {
	reason := "Error"
	if isTransient(err) {
		reason = "TransientErr"
	}
	r.recorder.Event(&am, corev1.EventTypeWarning, reason, strings.TrimSpace(err.Error()))
}

// Reconcile the given AtlasMigration resource.
func (r *AtlasMigrationReconciler) reconcile(
	ctx context.Context,
	md atlasMigrationData,
) (dbv1alpha1.AtlasMigrationStatus, error) {
	// Create atlas.hcl from template data
	atlasHCL, cleanUp, err := md.render()
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}
	defer cleanUp()

	// Calculate the observedHash
	hash, err := md.hash()
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}

	// Check if there are any pending migration files
	status, err := r.CLI.Status(ctx, &atlas.StatusParams{Env: md.EnvName, ConfigURL: atlasHCL})
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, transient(err)
	}
	if len(status.Pending) == 0 {
		var lastApplied int64
		if len(status.Applied) > 0 {
			lastApplied = status.Applied[len(status.Applied)-1].ExecutedAt.Unix()
		}
		return dbv1alpha1.AtlasMigrationStatus{
			ObservedHash:       hash,
			LastApplied:        lastApplied,
			LastAppliedVersion: status.Current,
		}, nil
	}

	// Execute Atlas CLI migrate command
	report, err := r.CLI.Apply(ctx, &atlas.ApplyParams{Env: md.EnvName, ConfigURL: atlasHCL})
	if err != nil {
		return dbv1alpha1.AtlasMigrationStatus{}, transient(err)
	}
	if report != nil && report.Error != "" {
		err = errors.New(report.Error)
		if !isSQLErr(err) {
			err = transient(err)
		}
		return dbv1alpha1.AtlasMigrationStatus{}, err
	}
	return dbv1alpha1.AtlasMigrationStatus{
		ObservedHash:       hash,
		LastApplied:        report.End.Unix(),
		LastAppliedVersion: report.Target,
	}, nil
}

// Extract migration data from the given resource
func (r *AtlasMigrationReconciler) extractMigrationData(
	ctx context.Context,
	am dbv1alpha1.AtlasMigration,
) (atlasMigrationData, func() error, error) {
	var (
		tmplData atlasMigrationData
		err      error
		creds    = am.Spec.Credentials
	)

	// Get database connection string
	switch {
	case am.Spec.URL != "":
		tmplData.URL = am.Spec.URL
	case am.Spec.URLFrom.SecretKeyRef != nil:
		tmplData.URL, err = getSecretValue(ctx, r, am.Namespace, *am.Spec.URLFrom.SecretKeyRef)
		if err != nil {
			return tmplData, nil, err
		}
	case creds.Host != "":
		if err := hydrateCredentials(ctx, &creds, r, am.Namespace); err != nil {
			return tmplData, nil, err
		}
		tmplData.URL = creds.URL().String()
	}

	// Get temporary directory
	cleanUpDir := func() error { return nil }
	if c := am.Spec.Dir.ConfigMapRef; c != nil {
		tmplData.Migration = &migration{}
		tmplData.Migration.Dir, cleanUpDir, err = r.createTmpDirFromCfgMap(ctx, am.Namespace, c.Name)
		if err != nil {
			return tmplData, nil, err
		}
	}

	// Get temporary directory in case of local directory
	if m := am.Spec.Dir.Local; m != nil {
		if tmplData.Migration != nil {
			return tmplData, nil, errors.New("cannot define both configmap and local directory")
		}

		tmplData.Migration = &migration{}
		tmplData.Migration.Dir, cleanUpDir, err = r.createTmpDirFromMap(ctx, m)
		if err != nil {
			return tmplData, nil, err
		}
	}

	// Get Atlas Cloud Token from secret
	if am.Spec.Cloud.TokenFrom.SecretKeyRef != nil {
		tmplData.Cloud = &cloud{
			URL:     am.Spec.Cloud.URL,
			Project: am.Spec.Cloud.Project,
		}

		if am.Spec.Dir.Remote.Name != "" {
			tmplData.Cloud.RemoteDir = &remoteDir{
				Name: am.Spec.Dir.Remote.Name,
				Tag:  am.Spec.Dir.Remote.Tag,
			}
		}

		tmplData.Cloud.Token, err = getSecretValue(ctx, r, am.Namespace, *am.Spec.Cloud.TokenFrom.SecretKeyRef)
		if err != nil {
			return tmplData, nil, err
		}
	}

	// Mapping EnvName, default to "kubernetes"
	tmplData.EnvName = am.Spec.EnvName
	if tmplData.EnvName == "" {
		tmplData.EnvName = "kubernetes"
	}

	tmplData.RevisionsSchema = am.Spec.RevisionsSchema
	return tmplData, cleanUpDir, nil
}

// createTmpDirFromCM creates a temporary directory by configmap
func (r *AtlasMigrationReconciler) createTmpDirFromCfgMap(
	ctx context.Context,
	ns, cfgName string,
) (string, func() error, error) {

	// Get configmap
	configMap := corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: ns,
		Name:      cfgName,
	}, &configMap); err != nil {
		return "", nil, transient(err)
	}

	return r.createTmpDirFromMap(ctx, configMap.Data)
}

// createTmpDirFromCM creates a temporary directory by configmap
func (r *AtlasMigrationReconciler) createTmpDirFromMap(
	ctx context.Context,
	m map[string]string,
) (string, func() error, error) {

	// Create temporary directory and remove it at the end of the function
	tmpDir, err := ioutil.TempDir("", "migrations")
	if err != nil {
		return "", nil, err
	}

	// Foreach configmap to build temporary directory
	// key is the name of the file and value is the content of the file
	for key, value := range m {
		filePath := filepath.Join(tmpDir, key)
		err := ioutil.WriteFile(filePath, []byte(value), 0644)
		if err != nil {
			// Remove the temporary directory if there is an error
			os.RemoveAll(tmpDir)
			return "", nil, err
		}
	}

	return fmt.Sprintf("file://%s", tmpDir), func() error {
		return os.RemoveAll(tmpDir)
	}, nil
}

func (r *AtlasMigrationReconciler) watch(am dbv1alpha1.AtlasMigration) {
	if c := am.Spec.Dir.ConfigMapRef; c != nil {
		r.configMapWatcher.Watch(
			types.NamespacedName{Name: c.Name, Namespace: am.Namespace},
			am.NamespacedName(),
		)
	}
	if s := am.Spec.Cloud.TokenFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: am.Namespace},
			am.NamespacedName(),
		)
	}
	if s := am.Spec.URLFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: am.Namespace},
			am.NamespacedName(),
		)
	}
	if s := am.Spec.Credentials.PasswordFrom.SecretKeyRef; s != nil {
		r.secretWatcher.Watch(
			types.NamespacedName{Name: s.Name, Namespace: am.Namespace},
			am.NamespacedName(),
		)
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *AtlasMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.AtlasMigration{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&dbv1alpha1.AtlasMigration{}).
		Watches(&source.Kind{Type: &corev1.Secret{}}, r.secretWatcher).
		Watches(&source.Kind{Type: &corev1.ConfigMap{}}, r.configMapWatcher).
		Complete(r)
}

// Render atlas.hcl file from the given data
func (amd atlasMigrationData) render() (string, func() error, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "atlas_migration.tmpl", amd); err != nil {
		return "", nil, err
	}

	return atlas.TempFile(buf.String(), "hcl")
}

// Calculate the hash of the given data
func (amd atlasMigrationData) hash() (string, error) {
	h := sha256.New()

	// Hash cloud directory
	h.Write([]byte(amd.URL))
	if amd.Cloud != nil {
		h.Write([]byte(amd.Cloud.Token))
		h.Write([]byte(amd.Cloud.URL))
		h.Write([]byte(amd.Cloud.Project))
		if amd.Cloud.RemoteDir != nil {
			h.Write([]byte(amd.Cloud.RemoteDir.Name))
			h.Write([]byte(amd.Cloud.RemoteDir.Tag))
			return hex.EncodeToString(h.Sum(nil)), nil
		}
	}

	// Hash local directory
	if amd.Migration != nil {
		u, err := url.Parse(amd.Migration.Dir)
		if err != nil {
			return "", err
		}
		d, err := migrate.NewLocalDir(u.Path)
		if err != nil {
			return "", err
		}
		hf, err := d.Checksum()
		if err != nil {
			return "", err
		}
		h.Write([]byte(hf.Sum()))
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
