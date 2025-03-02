/*
Copyright 2022.

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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/grafana-operator/grafana-operator/v5/embeds"

	"github.com/go-logr/logr"
	"github.com/grafana-operator/grafana-operator/v5/api/v1beta1"
	client2 "github.com/grafana-operator/grafana-operator/v5/controllers/client"
	"github.com/grafana-operator/grafana-operator/v5/controllers/fetchers"
	"github.com/grafana-operator/grafana-operator/v5/controllers/metrics"
	grapi "github.com/grafana/grafana-api-golang-client"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "k8s.io/api/core/v1"
)

const (
	initialSyncDelay = "10s"
	syncBatchSize    = 100
)

// GrafanaDashboardReconciler reconciles a GrafanaDashboard object
type GrafanaDashboardReconciler struct {
	Client    client.Client
	Log       logr.Logger
	Scheme    *runtime.Scheme
	Discovery discovery.DiscoveryInterface
}

//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadashboards,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadashboards/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanadashboards/finalizers,verbs=update

func (r *GrafanaDashboardReconciler) syncDashboards(ctx context.Context) (ctrl.Result, error) {
	syncLog := log.FromContext(ctx).WithName("GrafanaDashboardReconciler")
	dashboardsSynced := 0

	// get all grafana instances
	grafanas := &v1beta1.GrafanaList{}
	var opts []client.ListOption
	err := r.Client.List(ctx, grafanas, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// no instances, no need to sync
	if len(grafanas.Items) == 0 {
		return ctrl.Result{Requeue: false}, nil
	}

	// get all dashboards
	allDashboards := &v1beta1.GrafanaDashboardList{}
	err = r.Client.List(ctx, allDashboards, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	dashboardsToDelete := getDashboardsToDelete(allDashboards, grafanas.Items)

	// delete all dashboards that no longer have a cr
	for grafana, dashboards := range dashboardsToDelete {
		grafanaClient, err := client2.NewGrafanaClient(ctx, r.Client, grafana)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		for _, dashboard := range dashboards {
			// avoid bombarding the grafana instance with a large number of requests at once, limit
			// the sync to a certain number of dashboards per cycle. This means that it will take longer to sync
			// a large number of deleted dashboard crs, but that should be an edge case.
			if dashboardsSynced >= syncBatchSize {
				return ctrl.Result{Requeue: true}, nil
			}

			namespace, name, uid := dashboard.Split()
			err = grafanaClient.DeleteDashboardByUID(uid)
			if err != nil {
				if strings.Contains(err.Error(), "status: 404") {
					syncLog.Info("dashboard no longer exists", "namespace", namespace, "name", name)
				} else {
					return ctrl.Result{Requeue: false}, err
				}
			}

			grafana.Status.Dashboards = grafana.Status.Dashboards.Remove(namespace, name)
			dashboardsSynced += 1
		}

		// one update per grafana - this will trigger a reconcile of the grafana controller
		// so we should minimize those updates
		err = r.Client.Status().Update(ctx, grafana)
		if err != nil {
			return ctrl.Result{Requeue: false}, err
		}
	}

	if dashboardsSynced > 0 {
		syncLog.Info("successfully synced dashboards", "dashboards", dashboardsSynced)
	}
	return ctrl.Result{Requeue: false}, nil
}

// sync dashboards, delete dashboards from grafana that do no longer have a cr
func getDashboardsToDelete(allDashboards *v1beta1.GrafanaDashboardList, grafanas []v1beta1.Grafana) map[*v1beta1.Grafana][]v1beta1.NamespacedResource {
	dashboardsToDelete := map[*v1beta1.Grafana][]v1beta1.NamespacedResource{}
	for _, grafana := range grafanas {
		grafana := grafana
		for _, dashboard := range grafana.Status.Dashboards {
			if allDashboards.Find(dashboard.Namespace(), dashboard.Name()) == nil {
				dashboardsToDelete[&grafana] = append(dashboardsToDelete[&grafana], dashboard)
			}
		}
	}
	return dashboardsToDelete
}

func (r *GrafanaDashboardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	controllerLog := log.FromContext(ctx).WithName("GrafanaDashboardReconciler")
	r.Log = controllerLog

	// periodic sync reconcile
	if req.Namespace == "" && req.Name == "" {
		start := time.Now()
		syncResult, err := r.syncDashboards(ctx)
		elapsed := time.Since(start).Milliseconds()
		metrics.InitialDashboardSyncDuration.Set(float64(elapsed))
		return syncResult, err
	}

	cr := &v1beta1.GrafanaDashboard{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			err = r.onDashboardDeleted(ctx, req.Namespace, req.Name)
			if err != nil {
				return ctrl.Result{RequeueAfter: RequeueDelay}, err
			}
			return ctrl.Result{}, nil
		}
		controllerLog.Error(err, "error getting grafana dashboard cr")
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	instances, err := r.GetMatchingDashboardInstances(ctx, cr, r.Client)
	if err != nil {
		controllerLog.Error(err, "could not find matching instances", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	controllerLog.Info("found matching Grafana instances for dashboard", "count", len(instances.Items))

	dashboardJson, err := r.fetchDashboardJson(ctx, cr)
	if err != nil {
		controllerLog.Error(err, "error fetching dashboard", "dashboard", cr.Name)
		return ctrl.Result{RequeueAfter: RequeueDelay}, nil
	}

	dashboardModel, hash, err := r.getDashboardModel(cr, dashboardJson)
	if err != nil {
		controllerLog.Error(err, "failed to prepare dashboard model", "dashboard", cr.Name)
		return ctrl.Result{RequeueAfter: RequeueDelay}, nil
	}

	uid := fmt.Sprintf("%s", dashboardModel["uid"])

	// Garbage collection for a case where dashboard uid get changed, dashboard creation is expected to happen in a separate reconcilication cycle
	if cr.IsUpdatedUID(uid) {
		controllerLog.Info("dashboard uid got updated, deleting dashboards with the old uid")
		err = r.onDashboardDeleted(ctx, req.Namespace, req.Name)
		if err != nil {
			return ctrl.Result{RequeueAfter: RequeueDelay}, err
		}

		// Clean up uid, so further reconcilications can track changes there
		cr.Status.UID = ""
		err = r.Client.Status().Update(ctx, cr)
		if err != nil {
			return ctrl.Result{RequeueAfter: RequeueDelay}, err
		}

		// Status update should trigger the next reconciliation right away, no need to requeue for dashboard creation
		return ctrl.Result{}, nil
	}

	success := true
	for _, grafana := range instances.Items {
		// check if this is a cross namespace import
		if grafana.Namespace != cr.Namespace && !cr.IsAllowCrossNamespaceImport() {
			continue
		}

		grafana := grafana
		// an admin url is required to interact with grafana
		// the instance or route might not yet be ready
		if grafana.Status.Stage != v1beta1.OperatorStageComplete || grafana.Status.StageStatus != v1beta1.OperatorStageResultSuccess {
			controllerLog.Info("grafana instance not ready", "grafana", grafana.Name)
			success = false
			continue
		}

		if grafana.IsInternal() {
			// first reconcile the plugins
			// append the requested dashboards to a configmap from where the
			// grafana reconciler will pick them up
			err = ReconcilePlugins(ctx, r.Client, r.Scheme, &grafana, cr.Spec.Plugins, fmt.Sprintf("%v-dashboard", cr.Name))
			if err != nil {
				controllerLog.Error(err, "error reconciling plugins", "dashboard", cr.Name, "grafana", grafana.Name)
				success = false
			}
		}

		// then import the dashboard into the matching grafana instances
		err = r.onDashboardCreated(ctx, &grafana, cr, dashboardModel, hash)
		if err != nil {
			controllerLog.Error(err, "error reconciling dashboard", "dashboard", cr.Name, "grafana", grafana.Name)
			success = false
		}
	}

	// if the dashboard was successfully synced in all instances, wait for its re-sync period
	if success {
		if cr.ResyncPeriodHasElapsed() {
			cr.Status.LastResync = metav1.Time{Time: time.Now()}
		}
		cr.Status.Hash = hash
		cr.Status.UID = uid
		return ctrl.Result{RequeueAfter: cr.GetResyncPeriod()}, r.Client.Status().Update(ctx, cr)
	}

	return ctrl.Result{RequeueAfter: RequeueDelay}, nil
}

func (r *GrafanaDashboardReconciler) onDashboardDeleted(ctx context.Context, namespace string, name string) error {
	list := v1beta1.GrafanaList{}
	var opts []client.ListOption
	err := r.Client.List(ctx, &list, opts...)
	if err != nil {
		return err
	}

	for _, grafana := range list.Items {
		if found, uid := grafana.Status.Dashboards.Find(namespace, name); found {
			grafana := grafana
			grafanaClient, err := client2.NewGrafanaClient(ctx, r.Client, &grafana)
			if err != nil {
				return err
			}

			dash, err := grafanaClient.DashboardByUID(*uid)
			if err != nil {
				if !strings.Contains(err.Error(), "status: 404") {
					return err
				}
			}

			err = grafanaClient.DeleteDashboardByUID(*uid)
			if err != nil {
				if !strings.Contains(err.Error(), "status: 404") {
					return err
				}
			}

			if dash != nil && dash.Meta.Folder > 0 {
				resp, err := r.DeleteFolderIfEmpty(grafanaClient, dash.FolderID)
				if err != nil {
					return err
				}
				if resp.StatusCode == 200 {
					r.Log.Info("unused folder successfully removed")
				}
				if resp.StatusCode == 432 {
					r.Log.Info("folder still in use by other dashboards")
				}
			}

			if grafana.IsInternal() {
				err = ReconcilePlugins(ctx, r.Client, r.Scheme, &grafana, nil, fmt.Sprintf("%v-dashboard", name))
				if err != nil {
					return err
				}
			}

			grafana.Status.Dashboards = grafana.Status.Dashboards.Remove(namespace, name)
			err = r.Client.Status().Update(ctx, &grafana)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *GrafanaDashboardReconciler) onDashboardCreated(ctx context.Context, grafana *v1beta1.Grafana, cr *v1beta1.GrafanaDashboard, dashboardModel map[string]interface{}, hash string) error {
	if grafana.IsExternal() && cr.Spec.Plugins != nil {
		return fmt.Errorf("external grafana instances don't support plugins, please remove spec.plugins from your dashboard cr")
	}

	grafanaClient, err := client2.NewGrafanaClient(ctx, r.Client, grafana)
	if err != nil {
		return err
	}

	folderID, err := r.GetOrCreateFolder(grafanaClient, cr)
	if err != nil {
		return errors.NewInternalError(err)
	}

	uid := fmt.Sprintf("%s", dashboardModel["uid"])
	title := fmt.Sprintf("%s", dashboardModel["title"])

	exists, remoteUID, err := r.Exists(grafanaClient, uid, title, folderID)
	if err != nil {
		return err
	}

	if exists && remoteUID != uid {
		// If there's already a dashboard with the same title in the same folder, grafana preserves that dashboard's uid, so we should remove it first
		r.Log.Info("found dashboard with the same title (in the same folder) but different uid, removing the dashboard before recreating it with a new uid")
		err = grafanaClient.DeleteDashboardByUID(remoteUID)
		if err != nil {
			if !strings.Contains(err.Error(), "status: 404") {
				return err
			}
		}

		exists = false
	}

	if exists && cr.Unchanged(hash) && !cr.ResyncPeriodHasElapsed() {
		return nil
	}

	resp, err := grafanaClient.NewDashboard(grapi.Dashboard{
		Meta: grapi.DashboardMeta{
			IsStarred: false,
			Slug:      cr.Name,
			Folder:    folderID,
		},
		Model:     dashboardModel,
		FolderID:  folderID,
		Overwrite: true,
		Message:   "",
	})
	if err != nil {
		return err
	}

	if resp.Status != "success" {
		return errors.NewBadRequest(fmt.Sprintf("error creating dashboard, status was %v", resp.Status))
	}

	grafana.Status.Dashboards = grafana.Status.Dashboards.Add(cr.Namespace, cr.Name, uid)
	return r.Client.Status().Update(ctx, grafana)
}

// map data sources that are required in the dashboard to data sources that exist in the instance
func (r *GrafanaDashboardReconciler) resolveDatasources(dashboard *v1beta1.GrafanaDashboard, dashboardJson []byte) ([]byte, error) {
	if len(dashboard.Spec.Datasources) == 0 {
		return dashboardJson, nil
	}

	for _, input := range dashboard.Spec.Datasources {
		if input.DatasourceName == "" || input.InputName == "" {
			return nil, fmt.Errorf("invalid datasource input rule in dashboard %v/%v, input or datasource empty", dashboard.Namespace, dashboard.Name)
		}

		searchValue := fmt.Sprintf("${%s}", input.InputName)
		dashboardJson = bytes.ReplaceAll(dashboardJson, []byte(searchValue), []byte(input.DatasourceName))
	}

	return dashboardJson, nil
}

// fetchDashboardJson delegates obtaining the dashboard json definition to one of the known fetchers, for example
// from embedded raw json or from a url
func (r *GrafanaDashboardReconciler) fetchDashboardJson(ctx context.Context, dashboard *v1beta1.GrafanaDashboard) ([]byte, error) {
	sourceTypes := dashboard.GetSourceTypes()

	if len(sourceTypes) == 0 {
		return nil, fmt.Errorf("no source type provided for dashboard %v", dashboard.Name)
	}

	if len(sourceTypes) > 1 {
		return nil, fmt.Errorf("more than one source types found for dashboard %v", dashboard.Name)
	}

	switch sourceTypes[0] {
	case v1beta1.DashboardSourceTypeRawJson:
		return []byte(dashboard.Spec.Json), nil
	case v1beta1.DashboardSourceTypeGzipJson:
		return v1beta1.Gunzip([]byte(dashboard.Spec.GzipJson))
	case v1beta1.DashboardSourceTypeUrl:
		return fetchers.FetchDashboardFromUrl(dashboard)
	case v1beta1.DashboardSourceTypeJsonnet:
		envs := make(map[string]string)
		if dashboard.Spec.EnvsFrom != nil {
			for _, ref := range dashboard.Spec.EnvsFrom {
				key, val, err := r.getReferencedValue(ctx, dashboard, ref)
				if err != nil {
					return nil, fmt.Errorf("something went wrong processing envs, error: %w", err)
				}
				envs[key] = val
			}
		}
		if dashboard.Spec.Envs != nil {
			for _, ref := range dashboard.Spec.Envs {
				envs[ref.Name] = ref.Value
			}
		}
		return fetchers.FetchJsonnet(dashboard, envs, embeds.GrafonnetEmbed)
	case v1beta1.DashboardSourceTypeGrafanaCom:
		return fetchers.FetchDashboardFromGrafanaCom(dashboard)
	case v1beta1.DashboardSourceConfigMap:
		return fetchers.FetchDashboardFromConfigMap(dashboard, r.Client)
	default:
		return nil, fmt.Errorf("unknown source type %v found in dashboard %v", sourceTypes[0], dashboard.Name)
	}
}

func (r *GrafanaDashboardReconciler) getReferencedValue(ctx context.Context, cr *v1beta1.GrafanaDashboard, source v1beta1.GrafanaDashboardEnvFromSource) (string, string, error) {
	if source.SecretKeyRef != nil {
		s := &v1.Secret{}
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: source.SecretKeyRef.Name}, s)
		if err != nil {
			return "", "", err
		}
		if val, ok := s.Data[source.SecretKeyRef.Key]; ok {
			return source.SecretKeyRef.Key, string(val), nil
		} else {
			return "", "", fmt.Errorf("missing key %s in secret %s", source.SecretKeyRef.Key, source.ConfigMapKeyRef.Name)
		}
	} else {
		s := &v1.ConfigMap{}
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: source.SecretKeyRef.Name}, s)
		if err != nil {
			return "", "", err
		}
		if val, ok := s.Data[source.SecretKeyRef.Key]; ok {
			return source.SecretKeyRef.Key, val, nil
		} else {
			return "", "", fmt.Errorf("missing key %s in configmap %s", source.SecretKeyRef.Key, source.ConfigMapKeyRef.Name)
		}
	}
}

// getDashboardModel resolves datasources, updates uid (if needed) and converts raw json to type grafana client accepts
func (r *GrafanaDashboardReconciler) getDashboardModel(cr *v1beta1.GrafanaDashboard, dashboardJson []byte) (map[string]interface{}, string, error) {
	dashboardJson, err := r.resolveDatasources(cr, dashboardJson)
	if err != nil {
		return map[string]interface{}{}, "", err
	}

	hash := sha256.New()
	hash.Write(dashboardJson)

	var dashboardModel map[string]interface{}
	err = json.Unmarshal(dashboardJson, &dashboardModel)
	if err != nil {
		return map[string]interface{}{}, "", err
	}

	// NOTE: id should never be hardcoded in a dashboard, otherwise grafana will try to update a dashboard by id instead of uid.
	//       And, in case the id is non-existent, grafana will respond with 404. https://github.com/grafana-operator/grafana-operator/issues/1108
	dashboardModel["id"] = nil

	uid, _ := dashboardModel["uid"].(string) //nolint:errcheck
	if uid == "" {
		uid = string(cr.UID)
	}

	dashboardModel["uid"] = uid

	return dashboardModel, fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func (r *GrafanaDashboardReconciler) Exists(client *grapi.Client, uid string, title string, folderID int64) (bool, string, error) {
	dashboards, err := client.Dashboards()
	if err != nil {
		return false, "", err
	}

	for _, dashboard := range dashboards {
		if dashboard.UID == uid || (dashboard.Title == title && dashboard.FolderID == uint(folderID)) {
			return true, dashboard.UID, nil
		}
	}

	return false, "", nil
}

func (r *GrafanaDashboardReconciler) GetOrCreateFolder(client *grapi.Client, cr *v1beta1.GrafanaDashboard) (int64, error) {
	title := cr.Namespace
	if cr.Spec.FolderTitle != "" {
		title = cr.Spec.FolderTitle
	}

	exists, folderID, err := r.GetFolderID(client, title)
	if err != nil {
		return 0, err
	}

	if exists {
		return folderID, nil
	}

	// Folder wasn't found, let's create it
	resp, err := client.NewFolder(title)
	if err != nil {
		return 0, err
	}

	return resp.ID, nil
}

func (r *GrafanaDashboardReconciler) GetFolderID(client *grapi.Client,
	title string,
) (bool, int64, error) {
	// Pre-existing folder that is not returned in Folder API
	if strings.EqualFold(title, "General") {
		return true, 0, nil
	}

	folders, err := client.Folders()
	if err != nil {
		return false, 0, err
	}

	for _, folder := range folders {
		if strings.EqualFold(folder.Title, title) {
			return true, folder.ID, nil
		}
	}

	return false, 0, nil
}

func (r *GrafanaDashboardReconciler) DeleteFolderIfEmpty(client *grapi.Client, folderID int64) (http.Response, error) {
	dashboards, err := client.Dashboards()
	if err != nil {
		return http.Response{
			Status:     "internal grafana client error getting dashboards",
			StatusCode: 500,
		}, err
	}

	for _, dashboard := range dashboards {
		if int64(dashboard.FolderID) == folderID {
			return http.Response{
				Status:     "resource is still in use",
				StatusCode: 423, // Locked return code
			}, err
		}
		continue
	}

	folder, err := client.Folder(folderID)
	if err != nil {
		return http.Response{
			Status:     "internal grafana client error getting folder UID for folder",
			StatusCode: 500,
		}, err
	}

	if err = client.DeleteFolder(folder.UID); err != nil {
		return http.Response{
			Status:     "internal grafana client error deleting grafana folder",
			StatusCode: 500,
		}, err
	}
	return http.Response{
		Status:     "grafana folder deleted",
		StatusCode: 200,
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GrafanaDashboardReconciler) SetupWithManager(mgr ctrl.Manager, ctx context.Context) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.GrafanaDashboard{}).
		Complete(r)

	if err == nil {
		d, err := time.ParseDuration(initialSyncDelay)
		if err != nil {
			return err
		}

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
					result, err := r.Reconcile(ctx, ctrl.Request{})
					if err != nil {
						r.Log.Error(err, "error synchronizing dashboards")
						continue
					}
					if result.Requeue {
						r.Log.Info("more dashboards left to synchronize")
						continue
					}
					r.Log.Info("dashboard sync complete")
					return
				}
			}
		}()
	}

	return err
}

func (r *GrafanaDashboardReconciler) GetMatchingDashboardInstances(ctx context.Context, dashboard *v1beta1.GrafanaDashboard, k8sClient client.Client) (v1beta1.GrafanaList, error) {
	instances, err := GetMatchingInstances(ctx, k8sClient, dashboard.Spec.InstanceSelector)
	if err != nil || len(instances.Items) == 0 {
		dashboard.Status.NoMatchingInstances = true
		if err := r.Client.Status().Update(ctx, dashboard); err != nil {
			r.Log.Info("unable to update the status of %v, in %v", dashboard.Name, dashboard.Namespace)
		}
		return v1beta1.GrafanaList{}, err
	}
	dashboard.Status.NoMatchingInstances = false
	if err := r.Client.Status().Update(ctx, dashboard); err != nil {
		r.Log.Info("unable to update the status of %v, in %v", dashboard.Name, dashboard.Namespace)
	}

	return instances, err
}
