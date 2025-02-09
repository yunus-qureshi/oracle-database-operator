/*
** Copyright (c) 2021 Oracle and/or its affiliates.
**
** The Universal Permissive License (UPL), Version 1.0
**
** Subject to the condition set forth below, permission is hereby granted to any
** person obtaining a copy of this software, associated documentation and/or data
** (collectively the "Software"), free of charge and under any and all copyright
** rights in the Software, and any and all patent rights owned or freely
** licensable by each licensor hereunder covering either (i) the unmodified
** Software as contributed to or provided by such licensor, or (ii) the Larger
** Works (as defined below), to deal in both
**
** (a) the Software, and
** (b) any piece of software and/or hardware listed in the lrgrwrks.txt file if
** one is included with the Software (each a "Larger Work" to which the Software
** is contributed by such licensors),
**
** without restriction, including without limitation the rights to copy, create
** derivative works of, display, perform, and distribute the Software and make,
** use, sell, offer for sale, import, export, have made, and have sold the
** Software and the Larger Work(s), and to sublicense the foregoing rights on
** either these or other terms.
**
** This license is subject to the following condition:
** The above copyright notice and either this complete permission notice or at
** a minimum a reference to the UPL must be included in all copies or
** substantial portions of the Software.
**
** THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
** IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
** FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
** AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
** LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
** OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
** SOFTWARE.
 */

package controllers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dbapi "github.com/oracle/oracle-database-operator/apis/database/v1alpha1"
	dbcommons "github.com/oracle/oracle-database-operator/commons/database"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// SingleInstanceDatabaseReconciler reconciles a SingleInstanceDatabase object
type SingleInstanceDatabaseReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Config   *rest.Config
	Recorder record.EventRecorder
}

// To requeue after 15 secs allowing graceful state changes
var requeueY ctrl.Result = ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second}
var requeueN ctrl.Result = ctrl.Result{}

const singleInstanceDatabaseFinalizer = "database.oracle.com/singleinstancedatabasefinalizer"

//+kubebuilder:rbac:groups=database.oracle.com,resources=singleinstancedatabases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=database.oracle.com,resources=singleinstancedatabases/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=database.oracle.com,resources=singleinstancedatabases/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods;pods/log;pods/exec;persistentvolumeclaims;services;nodes;events,verbs=create;delete;get;list;patch;update;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SingleInstanceDatabase object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *SingleInstanceDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	r.Log.Info("Reconcile requested")
	var result ctrl.Result
	var err error
	completed := false
	blocked := false

	singleInstanceDatabase := &dbapi.SingleInstanceDatabase{}
	cloneFromDatabase := &dbapi.SingleInstanceDatabase{}

	// Execute for every reconcile
	defer r.updateReconcileStatus(singleInstanceDatabase, ctx, &result, &err, &blocked, &completed)

	err = r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, singleInstanceDatabase)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Log.Info("Resource not found")
			return requeueN, nil
		}
		return requeueY, err
	}

	// Manage SingleInstanceDatabase Deletion
	result, err = r.manageSingleInstanceDatabaseDeletion(req, ctx, singleInstanceDatabase)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// First validate
	result, err = r.validate(singleInstanceDatabase, cloneFromDatabase, ctx, req)
	if result.Requeue {
		r.Log.Info("Spec validation failed, Reconcile queued")
		return result, nil
	}
	if err != nil {
		r.Log.Info("Spec validation failed")
		return result, nil
	}

	// Service creation
	result, err = r.createOrReplaceSVC(ctx, req, singleInstanceDatabase)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// PVC Creation
	result, err = r.createOrReplacePVC(ctx, req, singleInstanceDatabase)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// POD creation
	result, err = r.createOrReplacePods(singleInstanceDatabase, cloneFromDatabase, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	if singleInstanceDatabase.Status.DatafilesCreated != "true" {
		// Creation of Oracle Wallet for Single Instance Database credentials
		result, err = r.createWallet(singleInstanceDatabase, ctx, req)
		if result.Requeue {
			r.Log.Info("Reconcile queued")
			return result, nil
		}
		if err != nil {
			r.Log.Info("Spec validation failed")
			return result, nil
		}
	}

	// Validate readiness
	var readyPod corev1.Pod
	result, readyPod, err = r.validateDBReadiness(singleInstanceDatabase, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Post DB ready operations

	// Deleting the oracle wallet
	if singleInstanceDatabase.Status.DatafilesCreated == "true" {
		result, err = r.deleteWallet(singleInstanceDatabase, ctx, req)
		if result.Requeue {
			r.Log.Info("Reconcile queued")
			return result, nil
		}
	}

	// Update DB config
	result, err = r.updateDBConfig(singleInstanceDatabase, readyPod, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Update Init Parameters
	result, err = r.updateInitParameters(singleInstanceDatabase, readyPod, ctx, req)
	if result.Requeue {
		r.Log.Info("Reconcile queued")
		return result, nil
	}

	// Run Datapatch
	if singleInstanceDatabase.Status.DatafilesPatched != "true" {
		// add a blocking reconcile condition
		err = errors.New("processing datapatch execution")
		blocked = true
		r.updateReconcileStatus(singleInstanceDatabase, ctx, &result, &err, &blocked, &completed)
		result, err = r.runDatapatch(singleInstanceDatabase, readyPod, ctx, req)
		if result.Requeue {
			r.Log.Info("Reconcile queued")
			return result, nil
		}
	}

	// If LoadBalancer = true , ensure Connect String is updated
	if singleInstanceDatabase.Status.ConnectString == dbcommons.ValueUnavailable {
		return requeueY, nil
	}

	// update status to Ready after all operations succeed
	singleInstanceDatabase.Status.Status = dbcommons.StatusReady

	completed = true
	r.Log.Info("Reconcile completed")
	return requeueN, nil
}

//#############################################################################
//    Update each reconcile condtion/status
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) updateReconcileStatus(m *dbapi.SingleInstanceDatabase, ctx context.Context,
	result *ctrl.Result, err *error, blocked *bool, completed *bool) {

	errMsg := func() string {
		if *err != nil {
			return (*err).Error()
		}
		return "no reconcile errors"
	}()
	var condition metav1.Condition
	if *completed {
		condition = metav1.Condition{
			Type:               dbcommons.ReconcileCompelete,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: m.GetGeneration(),
			Reason:             dbcommons.ReconcileCompleteReason,
			Message:            errMsg,
			Status:             metav1.ConditionTrue,
		}
	} else if *blocked {
		condition = metav1.Condition{
			Type:               dbcommons.ReconcileBlocked,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: m.GetGeneration(),
			Reason:             dbcommons.ReconcileBlockedReason,
			Message:            errMsg,
			Status:             metav1.ConditionTrue,
		}
	} else if result.Requeue {
		condition = metav1.Condition{
			Type:               dbcommons.ReconcileQueued,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: m.GetGeneration(),
			Reason:             dbcommons.ReconcileQueuedReason,
			Message:            errMsg,
			Status:             metav1.ConditionTrue,
		}
	} else if *err != nil {
		condition = metav1.Condition{
			Type:               dbcommons.ReconcileError,
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: m.GetGeneration(),
			Reason:             dbcommons.ReconcileErrorReason,
			Message:            errMsg,
			Status:             metav1.ConditionTrue,
		}
	} else {
		return
	}
	if len(m.Status.Conditions) > 0 {
		meta.RemoveStatusCondition(&m.Status.Conditions, condition.Type)
	}
	meta.SetStatusCondition(&m.Status.Conditions, condition)
	// Always refresh status before a reconcile
	r.Status().Update(ctx, m)
}

//#############################################################################
//    Validate the CRD specs
//    m = SingleInstanceDatabase
//    n = CloneFromDatabase
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) validate(m *dbapi.SingleInstanceDatabase,
	n *dbapi.SingleInstanceDatabase, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var err error
	eventReason := "Spec Error"
	var eventMsgs []string

	//  If Express Edition , Ensure Replicas=1
	if m.Spec.Edition == "express" && m.Spec.Replicas != 1 {
		eventMsgs = append(eventMsgs, "XE supports only one replica")
	}
	//  If Block Volume , Ensure Replicas=1
	if m.Spec.Persistence.AccessMode == "ReadWriteOnce" && m.Spec.Replicas != 1 {
		eventMsgs = append(eventMsgs, "accessMode ReadWriteOnce supports only one replica")
	}
	if m.Status.Sid != "" && !strings.EqualFold(m.Spec.Sid, m.Status.Sid) {
		eventMsgs = append(eventMsgs, "sid cannot be updated")
	}
	edition := m.Spec.Edition
	if m.Spec.Edition == "" {
		edition = "Enterprise"
	}
	if m.Spec.CloneFrom == "" && m.Status.Edition != "" && !strings.EqualFold(m.Status.Edition, edition) {
		eventMsgs = append(eventMsgs, "edition cannot be updated")
	}
	if m.Status.Charset != "" && !strings.EqualFold(m.Status.Charset, m.Spec.Charset) {
		eventMsgs = append(eventMsgs, "charset cannot be updated")
	}
	if m.Status.Pdbname != "" && !strings.EqualFold(m.Status.Pdbname, m.Spec.Pdbname) {
		eventMsgs = append(eventMsgs, "pdbName cannot be updated")
	}
	if m.Status.CloneFrom != "" &&
		(m.Status.CloneFrom == dbcommons.NoCloneRef && m.Spec.CloneFrom != "" ||
			m.Status.CloneFrom != dbcommons.NoCloneRef && m.Status.CloneFrom != m.Spec.CloneFrom) {
		eventMsgs = append(eventMsgs, "cloneFrom cannot be updated")
	}
	if m.Spec.Edition == "express" && m.Spec.CloneFrom != "" {
		eventMsgs = append(eventMsgs, "cloning not supported for express edition")
	}
	if m.Status.OrdsReference != "" && m.Status.Persistence.AccessMode != "" && m.Status.Persistence != m.Spec.Persistence {
		eventMsgs = append(eventMsgs, "uninstall ORDS to change Peristence")
	}
	if len(eventMsgs) > 0 {
		r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, strings.Join(eventMsgs, ","))
		r.Log.Info(strings.Join(eventMsgs, "\n"))
		err = errors.New(strings.Join(eventMsgs, ","))
		return requeueN, err
	}

	// Validating the secret
	if m.Status.DatafilesCreated != "true" {
		secret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: m.Spec.AdminPassword.SecretName, Namespace: m.Namespace}, secret)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Secret not found
				r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, err.Error())
				r.Log.Info(err.Error())
				m.Status.Status = dbcommons.StatusError
				r.Status().Update(ctx, m)
				return requeueY, err
			}
			r.Log.Error(err, "Unable to get the secret. Requeueing..")
			return requeueY, err
		}
	}

	// update status fields
	m.Status.Sid = m.Spec.Sid
	m.Status.Edition = strings.Title(edition)
	m.Status.Charset = m.Spec.Charset
	m.Status.Pdbname = m.Spec.Pdbname
	m.Status.Persistence = m.Spec.Persistence
	if m.Spec.CloneFrom == "" {
		m.Status.CloneFrom = dbcommons.NoCloneRef
	} else {
		m.Status.CloneFrom = m.Spec.CloneFrom
	}
	if m.Spec.CloneFrom != "" {
		// Once a clone database has created , it has no link with its reference
		if m.Status.DatafilesCreated == "true" {
			return requeueN, nil
		}
		// Fetch the Clone database reference
		err = r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: m.Spec.CloneFrom}, n)
		if err != nil {
			if apierrors.IsNotFound(err) {
				r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, err.Error())
				r.Log.Info(err.Error())
				return requeueN, err
			}
			return requeueY, err
		}

		if n.Status.Status != dbcommons.StatusReady {
			m.Status.Status = dbcommons.StatusPending
			eventReason := "Source Database Pending"
			eventMsg := "waiting for source database " + m.Spec.CloneFrom + " to be Ready"
			r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
			err = errors.New(eventMsg)
			return requeueY, err
		}

		if !n.Spec.ArchiveLog {
			m.Status.Status = dbcommons.StatusPending
			eventReason := "Source Database Pending"
			eventMsg := "waiting for ArchiveLog to turn ON " + n.Name
			r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
			r.Log.Info(eventMsg)
			err = errors.New(eventMsg)
			return requeueY, err
		}

		m.Status.Edition = n.Status.Edition

	}
	return requeueN, nil
}

//#############################################################################
//    Instantiate POD spec from SingleInstanceDatabase spec
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) instantiatePodSpec(m *dbapi.SingleInstanceDatabase, n *dbapi.SingleInstanceDatabase) *corev1.Pod {

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind: "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name + "-" + dbcommons.GenerateRandomString(5),
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app":     m.Name,
				"version": m.Spec.Image.Version,
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "datamount",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: m.Name,
						ReadOnly:  false,
					},
				},
			}},
			InitContainers: func() []corev1.Container {
				if m.Spec.Edition != "express" {
					return []corev1.Container{{
						Name:    "init-permissions",
						Image:   m.Spec.Image.PullFrom,
						Command: []string{"/bin/sh", "-c", fmt.Sprintf("chown %d:%d /opt/oracle/oradata", int(dbcommons.ORACLE_UID), int(dbcommons.ORACLE_GUID))},
						SecurityContext: &corev1.SecurityContext{
							// User ID 0 means, root user
							RunAsUser: func() *int64 { i := int64(0); return &i }(),
						},
						VolumeMounts: []corev1.VolumeMount{{
							MountPath: "/opt/oracle/oradata",
							Name:      "datamount",
						}},
					}, {
						Name:  "init-wallet",
						Image: m.Spec.Image.PullFrom,
						Env: []corev1.EnvVar{
							{
								Name:  "ORACLE_SID",
								Value: strings.ToUpper(m.Spec.Sid),
							},
							{
								Name:  "WALLET_CLI",
								Value: "mkstore",
							},
							{
								Name:  "WALLET_DIR",
								Value: "/opt/oracle/oradata/dbconfig/$(ORACLE_SID)/.wallet",
							},
						},
						Command: []string{"/bin/sh"},
						Args: func() []string {
							edition := ""
							if m.Spec.CloneFrom == "" {
								edition = m.Spec.Edition
								if m.Spec.Edition == "" {
									edition = "enterprise"
								}
							} else {
								edition = n.Spec.Edition
								if n.Spec.Edition == "" {
									edition = "enterprise"
								}
							}
							return []string{"-c", fmt.Sprintf(dbcommons.InitWalletCMD, edition)}
						}(),
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:  func() *int64 { i := int64(dbcommons.ORACLE_UID); return &i }(),
							RunAsGroup: func() *int64 { i := int64(dbcommons.ORACLE_GUID); return &i }(),
						},
						VolumeMounts: []corev1.VolumeMount{{
							MountPath: "/opt/oracle/oradata",
							Name:      "datamount",
						}},
					}}
				}
				return []corev1.Container{{
					Name:    "init-permissions",
					Image:   m.Spec.Image.PullFrom,
					Command: []string{"/bin/sh", "-c", fmt.Sprintf("chown %d:%d /opt/oracle/oradata", int(dbcommons.ORACLE_UID), int(dbcommons.ORACLE_GUID))},
					SecurityContext: &corev1.SecurityContext{
						// User ID 0 means, root user
						RunAsUser: func() *int64 { i := int64(0); return &i }(),
					},
					VolumeMounts: []corev1.VolumeMount{{
						MountPath: "/opt/oracle/oradata",
						Name:      "datamount",
					}},
				}}
			}(),
			Containers: []corev1.Container{{
				Name:  m.Name,
				Image: m.Spec.Image.PullFrom,
				Lifecycle: &corev1.Lifecycle{
					PreStop: &corev1.Handler{
						Exec: &corev1.ExecAction{
							Command: []string{"/bin/sh", "-c", "/bin/echo -en 'shutdown abort;\n' | env ORACLE_SID=${ORACLE_SID^^} sqlplus -S / as sysdba"},
						},
					},
				},
				ImagePullPolicy: corev1.PullAlways,
				Ports:           []corev1.ContainerPort{{ContainerPort: 1521}, {ContainerPort: 5500}},

				ReadinessProbe: &corev1.Probe{
					Handler: corev1.Handler{
						Exec: &corev1.ExecAction{
							Command: []string{"/bin/sh", "-c", "if [ -f $ORACLE_BASE/checkDBLockStatus.sh ]; then $ORACLE_BASE/checkDBLockStatus.sh ; else $ORACLE_BASE/checkDBStatus.sh; fi "},
						},
					},
					InitialDelaySeconds: 20,
					TimeoutSeconds:      20,
					PeriodSeconds: func() int32 {
						if m.Spec.ReadinessCheckPeriod > 0 {
							return int32(m.Spec.ReadinessCheckPeriod)
						}
						return 30
					}(),
				},

				VolumeMounts: []corev1.VolumeMount{{
					MountPath: "/opt/oracle/oradata",
					Name:      "datamount",
				}},
				Env: func() []corev1.EnvVar {
					if m.Spec.CloneFrom == "" {
						return []corev1.EnvVar{
							{
								Name:  "SVC_HOST",
								Value: m.Name,
							},
							{
								Name:  "SVC_PORT",
								Value: "1521",
							},
							{
								Name: "CREATE_PDB",
								Value: func() string {
									if m.Spec.Pdbname != "" {
										return "true"
									}
									return "false"
								}(),
							},
							{
								Name:  "ORACLE_SID",
								Value: strings.ToUpper(m.Spec.Sid),
							},
							{
								Name:  "WALLET_DIR",
								Value: "/opt/oracle/oradata/dbconfig/$(ORACLE_SID)/.wallet",
							},
							{
								Name:  "ORACLE_PDB",
								Value: m.Spec.Pdbname,
							},
							{
								Name:  "ORACLE_CHARACTERSET",
								Value: m.Spec.Charset,
							},
							{
								Name:  "ORACLE_EDITION",
								Value: m.Spec.Edition,
							},
							{
								Name: "INIT_SGA_SIZE",
								Value: func() string {
									if m.Spec.InitParams.SgaTarget > 0 && m.Spec.InitParams.PgaAggregateTarget > 0 {
										return strconv.Itoa(m.Spec.InitParams.SgaTarget)
									}
									return ""
								}(),
							},
							{
								Name: "INIT_PGA_SIZE",
								Value: func() string {
									if m.Spec.InitParams.SgaTarget > 0 && m.Spec.InitParams.PgaAggregateTarget > 0 {
										return strconv.Itoa(m.Spec.InitParams.SgaTarget)
									}
									return ""
								}(),
							},
							{
								Name:  "SKIP_DATAPATCH",
								Value: "true",
							},
						}
					}
					return []corev1.EnvVar{
						{
							Name:  "SVC_HOST",
							Value: m.Name,
						},
						{
							Name:  "SVC_PORT",
							Value: "1521",
						},
						{
							Name:  "ORACLE_SID",
							Value: strings.ToUpper(m.Spec.Sid),
						},
						{
							Name:  "WALLET_DIR",
							Value: "/opt/oracle/oradata/dbconfig/$(ORACLE_SID)/.wallet",
						},
						{
							Name:  "PRIMARY_DB_CONN_STR",
							Value: n.Name + ":1521/" + n.Spec.Sid,
						},
						{
							Name:  "PRIMARY_SID",
							Value: strings.ToUpper(n.Spec.Sid),
						},
						{
							Name:  "PRIMARY_NAME",
							Value: n.Name,
						},
						{
							Name: "ORACLE_HOSTNAME",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath: "status.podIP",
								},
							},
						},
						{
							Name:  "CLONE_DB",
							Value: "true",
						},
						{
							Name:  "SKIP_DATAPATCH",
							Value: "true",
						},
					}
				}(),
			}},

			TerminationGracePeriodSeconds: func() *int64 { i := int64(30); return &i }(),

			NodeSelector: func() map[string]string {
				ns := make(map[string]string)
				if len(m.Spec.NodeSelector) != 0 {
					for key, value := range m.Spec.NodeSelector {
						ns[key] = value
					}
				}
				return ns
			}(),

			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: func() *int64 {
					i := int64(0)
					if m.Spec.Edition != "express" {
						i = int64(dbcommons.ORACLE_UID)
					}
					return &i
				}(),
				RunAsGroup: func() *int64 {
					i := int64(0)
					if m.Spec.Edition != "express" {
						i = int64(dbcommons.ORACLE_GUID)
					}
					return &i
				}(),
			},
			ImagePullSecrets: []corev1.LocalObjectReference{
				{
					Name: m.Spec.Image.PullSecrets,
				},
			},
		},
	}

	// Set SingleInstanceDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, pod, r.Scheme)
	return pod
}

//#############################################################################
//    Instantiate Service spec from SingleInstanceDatabase spec
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) instantiateSVCSpec(m *dbapi.SingleInstanceDatabase) *corev1.Service {
	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind: "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app": m.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:     "listener",
					Port:     1521,
					Protocol: corev1.ProtocolTCP,
				},
				{
					Name:     "xmldb",
					Port:     5500,
					Protocol: corev1.ProtocolTCP,
				},
			},
			Selector: map[string]string{
				"app": m.Name,
			},
			Type: corev1.ServiceType(func() string {
				if m.Spec.LoadBalancer {
					return "LoadBalancer"
				}
				return "NodePort"
			}()),
		},
	}
	// Set SingleInstanceDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, svc, r.Scheme)
	return svc
}

//#############################################################################
//    Instantiate Persistent Volume Claim spec from SingleInstanceDatabase spec
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) instantiatePVCSpec(m *dbapi.SingleInstanceDatabase) *corev1.PersistentVolumeClaim {

	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			Kind: "PersistentVolumeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"app": m.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: func() []corev1.PersistentVolumeAccessMode {
				var accessMode []corev1.PersistentVolumeAccessMode
				accessMode = append(accessMode, corev1.PersistentVolumeAccessMode(m.Spec.Persistence.AccessMode))
				return accessMode
			}(),
			Resources: corev1.ResourceRequirements{
				Requests: map[corev1.ResourceName]resource.Quantity{
					// Requests describes the minimum amount of compute resources required
					"storage": resource.MustParse(m.Spec.Persistence.Size),
				},
			},
			StorageClassName: &m.Spec.Persistence.StorageClass,
		},
	}
	// Set SingleInstanceDatabase instance as the owner and controller
	ctrl.SetControllerReference(m, pvc, r.Scheme)
	return pvc
}

//#############################################################################
//    Stake a claim for Persistent Volume
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) createOrReplacePVC(ctx context.Context, req ctrl.Request,
	m *dbapi.SingleInstanceDatabase) (ctrl.Result, error) {

	log := r.Log.WithValues("createPVC", req.NamespacedName)

	pvcDeleted := false
	// Check if the PVC already exists using r.Get, if not create a new one using r.Create
	pvc := &corev1.PersistentVolumeClaim{}
	// Get retrieves an obj ( a struct pointer ) for the given object key from the Kubernetes Cluster.
	err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, pvc)
	if err == nil {
		if *pvc.Spec.StorageClassName != m.Spec.Persistence.StorageClass ||
			pvc.Spec.Resources.Requests["storage"] != resource.MustParse(m.Spec.Persistence.Size) ||
			pvc.Spec.AccessModes[0] != corev1.PersistentVolumeAccessMode(m.Spec.Persistence.AccessMode) {
			// call deletePods() with zero pods in avaiable and nil readyPod to delete all pods
			result, err := r.deletePods(ctx, req, m, []corev1.Pod{}, corev1.Pod{}, 0, 0)
			if result.Requeue {
				return result, err
			}

			log.Info("Deleting PVC", " name ", pvc.Name)
			err = r.Delete(ctx, pvc)
			if err != nil {
				r.Log.Error(err, "Failed to delete Pvc", "Pvc.Name", pvc.Name)
				return requeueN, err
			}
			pvcDeleted = true
		} else {
			log.Info("Found Existing PVC", "Name", pvc.Name)
			return requeueN, nil
		}
	}
	if pvcDeleted || err != nil && apierrors.IsNotFound(err) {
		// Define a new PVC
		pvc = r.instantiatePVCSpec(m)
		log.Info("Creating a new PVC", "PVC.Namespace", pvc.Namespace, "PVC.Name", pvc.Name)
		err = r.Create(ctx, pvc)
		if err != nil {
			log.Error(err, "Failed to create new PVC", "PVC.Namespace", pvc.Namespace, "PVC.Name", pvc.Name)
			return requeueY, err
		}
		return requeueN, nil
	} else if err != nil {
		log.Error(err, "Failed to get PVC")
		return requeueY, err
	}

	return requeueN, nil
}

//#############################################################################
//    Create a Service for SingleInstanceDatabase
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) createOrReplaceSVC(ctx context.Context, req ctrl.Request,
	m *dbapi.SingleInstanceDatabase) (ctrl.Result, error) {

	log := r.Log.WithValues("createOrReplaceSVC", req.NamespacedName)

	svcDeleted := false
	// Check if the Service already exists, if not create a new one
	svc := &corev1.Service{}
	// Get retrieves an obj ( a struct pointer ) for the given object key from the Kubernetes Cluster.
	err := r.Get(ctx, types.NamespacedName{Name: m.Name, Namespace: m.Namespace}, svc)
	if err == nil {
		svcType := corev1.ServiceType("NodePort")
		if m.Spec.LoadBalancer {
			svcType = corev1.ServiceType("LoadBalancer")
		}

		if svc.Spec.Type != svcType {
			log.Info("Deleting SVC", " name ", svc.Name)
			err = r.Delete(ctx, svc)
			if err != nil {
				r.Log.Error(err, "Failed to delete svc", " Name", svc.Name)
				return requeueN, err
			}
			svcDeleted = true
		}
	}
	if svcDeleted || err != nil && apierrors.IsNotFound(err) {
		// Define a new Service
		svc = r.instantiateSVCSpec(m)
		log.Info("Creating a new Service", "Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
		err = r.Create(ctx, svc)
		if err != nil {
			log.Error(err, "Failed to create new Service", "Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
			return requeueY, err
		}
	} else if err != nil {
		log.Error(err, "Failed to get Service")
		return requeueY, err
	}
	log.Info("Found Existing Service ", "Service Name ", svc.Name)

	m.Status.ConnectString = dbcommons.ValueUnavailable
	m.Status.PdbConnectString = dbcommons.ValueUnavailable
	m.Status.OemExpressUrl = dbcommons.ValueUnavailable
	pdbName := "ORCLPDB1"
	if m.Spec.Pdbname != "" {
		pdbName = strings.ToUpper(m.Spec.Pdbname)
	}
	if m.Spec.LoadBalancer {
		m.Status.ClusterConnectString = svc.Name + "." + svc.Namespace + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/" + strings.ToUpper(m.Spec.Sid)
		if len(svc.Status.LoadBalancer.Ingress) > 0 {
			m.Status.ConnectString = svc.Status.LoadBalancer.Ingress[0].IP + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/" + strings.ToUpper(m.Spec.Sid)
			m.Status.PdbConnectString = svc.Status.LoadBalancer.Ingress[0].IP + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/" + strings.ToUpper(pdbName)
			m.Status.OemExpressUrl = "https://" + svc.Status.LoadBalancer.Ingress[0].IP + ":" + fmt.Sprint(svc.Spec.Ports[1].Port) + "/em"
		}
		return requeueN, nil
	}

	m.Status.ClusterConnectString = svc.Name + "." + svc.Namespace + ":" + fmt.Sprint(svc.Spec.Ports[0].Port) + "/" + strings.ToUpper(m.Spec.Sid)
	nodeip := dbcommons.GetNodeIp(r, ctx, req)
	if nodeip != "" {
		m.Status.ConnectString = nodeip + ":" + fmt.Sprint(svc.Spec.Ports[0].NodePort) + "/" + strings.ToUpper(m.Spec.Sid)
		m.Status.PdbConnectString = nodeip + ":" + fmt.Sprint(svc.Spec.Ports[0].NodePort) + "/" + strings.ToUpper(pdbName)
		m.Status.OemExpressUrl = "https://" + nodeip + ":" + fmt.Sprint(svc.Spec.Ports[1].NodePort) + "/em"
	}

	return requeueN, nil
}

//#############################################################################
//    Create new Pods or delete old/extra pods
//    m = SingleInstanceDatabase
//    n = CloneFromDatabase
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) createOrReplacePods(m *dbapi.SingleInstanceDatabase, n *dbapi.SingleInstanceDatabase,
	ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("createOrReplacePods", req.NamespacedName)

	oldVersion := ""
	oldImage := ""

	// call FindPods() to fetch pods all version/images of the same SIDB kind
	readyPod, replicasFound, available, podsMarkedToBeDeleted, err := dbcommons.FindPods(r, "", "", m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, err
	}
	if m.Spec.Edition == "express" && podsMarkedToBeDeleted > 0 {
		// Recreate new pods only after earlier pods are terminated completely
		return requeueY, err
	}
	if readyPod.Name != "" {
		available = append(available, readyPod)
	}

	for _, pod := range available {
		if pod.Labels["version"] != m.Spec.Image.Version {
			oldVersion = pod.Labels["version"]
		}
		if pod.Spec.Containers[0].Image != m.Spec.Image.PullFrom {
			oldImage = pod.Spec.Containers[0].Image
		}

	}

	// podVersion, podImage if old version PODs are found
	imageChanged := oldVersion != "" || oldImage != ""

	if !imageChanged {
		eventReason := ""
		eventMsg := ""
		if replicasFound == m.Spec.Replicas {
			return requeueN, nil
		}
		if replicasFound < m.Spec.Replicas {
			if replicasFound != 0 {
				eventReason = "Scaling Out"
				eventMsg = "from " + strconv.Itoa(replicasFound) + " pods to " + strconv.Itoa(m.Spec.Replicas)
				r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
			}
			// If version is same , call createPods() with the same version ,  and no of Replicas required
			return r.createPods(m, n, ctx, req, replicasFound)
		}
		eventReason = "Scaling In"
		eventMsg = "from " + strconv.Itoa(replicasFound) + " pods to " + strconv.Itoa(m.Spec.Replicas)
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		// Delete extra PODs
		return r.deletePods(ctx, req, m, available, readyPod, replicasFound, m.Spec.Replicas)
	}

	// Version/Image changed
	// PATCHING START (Only Software Patch)
	// call FindPods() to find pods of newer version . if running , delete the older version replicas.
	readyPod, replicasFound, available, _, err = dbcommons.FindPods(r, m.Spec.Image.Version,
		m.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, nil
	}

	// create new Pods with the new Version and no.of Replicas required
	result, err := r.createPods(m, n, ctx, req, replicasFound)
	if result.Requeue {
		return result, err
	}

	// Findpods() only returns non ready pods
	if readyPod.Name != "" {
		log.Info("New ready pod found", "name", readyPod.Name)
		available = append(available, readyPod)
	}
	if ok, _ := dbcommons.IsAnyPodWithStatus(available, corev1.PodRunning); !ok {
		eventReason := "Database Pending"
		eventMsg := "waiting for newer version/image DB pods get to running state"
		r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)
		log.Info(eventMsg)
		return requeueY, errors.New(eventMsg)
	}

	// call FindPods() to find pods of older version . delete all the Pods
	readyPod, replicasFound, available, _, err = dbcommons.FindPods(r, oldVersion,
		oldImage, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, err
	}
	if readyPod.Name != "" {
		log.Info("Ready pod marked for deletion", "name", readyPod.Name)
		available = append(available, readyPod)
	}
	return r.deletePods(ctx, req, m, available, corev1.Pod{}, replicasFound, 0)
	// PATCHING END
}

//#############################################################################
//    Function for creating Oracle Wallet
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) createWallet(m *dbapi.SingleInstanceDatabase, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Wallet not supported for XE Database
	if m.Spec.Edition == "express" {
		return requeueN, nil
	}

	// Listing all the pods
	readyPod, _, availableFinal, _, err := dbcommons.FindPods(r, m.Spec.Image.Version,
		m.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, nil
	}
	if readyPod.Name != "" {
		return requeueN, nil
	}

	// Wallet is created in persistent volume, hence it only needs to be executed once for all number of pods
	if len(availableFinal) == 0 {
		r.Log.Info("Pods are being created, currently no pods available")
		return requeueY, nil
	}

	// Iterate through the avaialableFinal (list of pods) to find out the pod whose status is updated about the init containers
	// If no required pod found then requeue the reconcile request
	var pod corev1.Pod
	var podFound bool
	for _, pod = range availableFinal {
		// Check if pod status contianer is updated about init containers
		if len(pod.Status.InitContainerStatuses) > 0 {
			podFound = true
			break
		}
	}
	if !podFound {
		r.Log.Info("No pod has its status updated about init containers. Requeueing...")
		return requeueY, nil
	}

	lastInitContIndex := len(pod.Status.InitContainerStatuses) - 1

	// If InitContainerStatuses[<index_of_init_container>].Ready is true, it means that the init container is successful
	if pod.Status.InitContainerStatuses[lastInitContIndex].Ready {
		// Init container named "init-wallet" has completed it's execution, hence return and don't requeue
		return requeueN, nil
	}

	if pod.Status.InitContainerStatuses[lastInitContIndex].State.Running == nil {
		// Init container named "init-wallet" is not running, so waiting for it to come in running state requeueing the reconcile request
		r.Log.Info("Waiting for init-wallet to come in running state...")
		return requeueY, nil
	}

	if m.Spec.CloneFrom == "" && m.Spec.Edition != "express" {
		//Check if Edition of m.Spec.Sid is same as m.Spec.Edition
		getEditionFile := dbcommons.GetEnterpriseEditionFileCMD
		eventReason := m.Spec.Sid + " is a enterprise edition"
		if m.Spec.Edition == "enterprise" || m.Spec.Edition == "" {
			getEditionFile = dbcommons.GetStandardEditionFileCMD
			eventReason = m.Spec.Sid + " is a standard edition"
		}
		out, err := dbcommons.ExecCommand(r, r.Config, pod.Name, pod.Namespace, "init-wallet",
			ctx, req, false, "bash", "-c", getEditionFile)
		r.Log.Info("getEditionFile Output : \n" + out)

		if err == nil && out != "" {
			m.Status.Status = dbcommons.StatusError
			eventMsg := "wrong edition"
			r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)
			return requeueN, errors.New("wrong Edition")
		}
	}

	r.Log.Info("Creating Wallet...")

	// Querying the secret
	r.Log.Info("Querying the database secret ...")
	secret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: m.Spec.AdminPassword.SecretName, Namespace: m.Namespace}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Log.Info("Secret not found")
			m.Status.Status = dbcommons.StatusError
			r.Status().Update(ctx, m)
			return requeueY, nil
		}
		r.Log.Error(err, "Unable to get the secret. Requeueing..")
		return requeueY, nil
	}

	// Execing into the pods and creating the wallet
	adminPassword := string(secret.Data[m.Spec.AdminPassword.SecretKey])

	out, err := dbcommons.ExecCommand(r, r.Config, pod.Name, pod.Namespace, "init-wallet",
		ctx, req, true, "bash", "-c", fmt.Sprintf("%s && %s && %s",
			dbcommons.WalletPwdCMD,
			dbcommons.WalletCreateCMD,
			fmt.Sprintf(dbcommons.WalletEntriesCMD, adminPassword)))
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, nil
	}
	r.Log.Info("Creating wallet entry Output : \n" + out)

	return requeueN, nil
}

//#############################################################################
//    Create the requested POD replicas
//    m = SingleInstanceDatabase
//    n = CloneFromDatabase
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) createPods(m *dbapi.SingleInstanceDatabase, n *dbapi.SingleInstanceDatabase,
	ctx context.Context, req ctrl.Request, replicasFound int) (ctrl.Result, error) {

	log := r.Log.WithValues("createPods", req.NamespacedName)

	replicasReq := m.Spec.Replicas
	log.Info("Replica Info", "Found", replicasFound, "Required", replicasReq)
	if replicasFound == replicasReq {
		log.Info("No of " + m.Name + " replicas found are same as required")
		return requeueN, nil
	}
	if replicasFound == 0 {
		m.Status.Status = dbcommons.StatusPending
		m.Status.DatafilesCreated = "false"
		m.Status.DatafilesPatched = "false"
		m.Status.Role = dbcommons.ValueUnavailable
		m.Status.ConnectString = dbcommons.ValueUnavailable
		m.Status.PdbConnectString = dbcommons.ValueUnavailable
		m.Status.OemExpressUrl = dbcommons.ValueUnavailable
		m.Status.ReleaseUpdate = dbcommons.ValueUnavailable
	}
	//  if Found < Required , Create New Pods , Name of Pods are generated Randomly
	for i := replicasFound; i < replicasReq; i++ {
		pod := r.instantiatePodSpec(m, n)
		log.Info("Creating a new "+m.Name+" POD", "POD.Namespace", pod.Namespace, "POD.Name", pod.Name)
		err := r.Create(ctx, pod)
		if err != nil {
			log.Error(err, "Failed to create new "+m.Name+" POD", "pod.Namespace", pod.Namespace, "POD.Name", pod.Name)
			return requeueY, err
		}
	}

	readyPod, _, availableFinal, _, err := dbcommons.FindPods(r, m.Spec.Image.Version,
		m.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, err
	}
	if readyPod.Name != "" {
		availableFinal = append(availableFinal, readyPod)
	}

	m.Status.Replicas = m.Spec.Replicas

	podNamesFinal := dbcommons.GetPodNames(availableFinal)
	log.Info("Final "+m.Name+" Pods After Deleting (or) Adding Extra Pods ( Including The Ready Pod ) ", "Pod Names", podNamesFinal)
	log.Info(m.Name+" Replicas Available", "Count", len(podNamesFinal))
	log.Info(m.Name+" Replicas Required", "Count", replicasReq)

	return requeueN, nil
}

//#############################################################################
//    Create the requested POD replicas
//    m = SingleInstanceDatabase
//    n = CloneFromDatabase
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) deletePods(ctx context.Context, req ctrl.Request, m *dbapi.SingleInstanceDatabase, available []corev1.Pod,
	readyPod corev1.Pod, replicasFound int, replicasRequired int) (ctrl.Result, error) {
	log := r.Log.WithValues("deletePods", req.NamespacedName)

	var err error
	if len(available) == 0 {
		// As list of pods not avaiable . fetch them ( len(available) == 0 ; Usecase where deletion of all pods required )
		var readyPodToBeDeleted corev1.Pod
		readyPodToBeDeleted, replicasFound, available, _, err = dbcommons.FindPods(r, "",
			"", m.Name, m.Namespace, ctx, req)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		// Append readyPod to avaiable for deleting all pods
		if readyPodToBeDeleted.Name != "" {
			available = append(available, readyPodToBeDeleted)
		}
	}

	// For deleting all pods , call with readyPod as nil ( corev1.Pod{} ) and append readyPod to avaiable while calling deletePods()
	//  if Found > Required , Delete Extra Pods
	if replicasFound > len(available) {
		// if available does not contain readyPOD, add it
		available = append(available, readyPod)
	}

	noDeleted := 0
	for _, availablePod := range available {
		if readyPod.Name == availablePod.Name {
			continue
		}
		if replicasRequired == (len(available) - noDeleted) {
			break
		}
		r.Log.Info("Deleting Pod : ", "POD.NAME", availablePod.Name)
		err := r.Delete(ctx, &availablePod, &client.DeleteOptions{})
		noDeleted += 1
		if err != nil {
			r.Log.Error(err, "Failed to delete existing POD", "POD.Name", availablePod.Name)
			// Don't requeue
		}
	}

	m.Status.Replicas = m.Spec.Replicas

	return requeueN, nil
}

//#############################################################################
//    ValidateDBReadiness and return the ready POD
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) validateDBReadiness(m *dbapi.SingleInstanceDatabase,
	ctx context.Context, req ctrl.Request) (ctrl.Result, corev1.Pod, error) {

	readyPod, _, available, _, err := dbcommons.FindPods(r, m.Spec.Image.Version,
		m.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, readyPod, err
	}
	if readyPod.Name == "" {
		eventReason := "Database Pending"
		eventMsg := "waiting for database pod to be ready"
		m.Status.Status = dbcommons.StatusPending
		if ok, _ := dbcommons.IsAnyPodWithStatus(available, corev1.PodFailed); ok {
			eventReason = "Database Failed"
			eventMsg = "pod creation failed"
		} else if ok, runningPod := dbcommons.IsAnyPodWithStatus(available, corev1.PodRunning); ok {
			eventReason = "Database Creating"
			eventMsg = "waiting for database to be ready"
			m.Status.Status = dbcommons.StatusCreating
			if m.Spec.Edition == "express" {
				eventReason = "Database Unhealthy"
				m.Status.Status = dbcommons.StatusNotReady
			}
			out, err := dbcommons.ExecCommand(r, r.Config, runningPod.Name, runningPod.Namespace, "",
				ctx, req, false, "bash", "-c", dbcommons.GetCheckpointFileCMD)
			if err != nil {
				r.Log.Error(err, err.Error())
				return requeueY, readyPod, err
			}
			r.Log.Info("GetCheckpointFileCMD Output : \n" + out)

			if out != "" {
				eventReason = "Database Unhealthy"
				eventMsg = "datafiles exists"
				m.Status.DatafilesCreated = "true"
				m.Status.Status = dbcommons.StatusNotReady
			}

		}
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		r.Log.Info(eventMsg)
		// As No pod is ready now , turn on mode when pod is ready . so requeue the request
		return requeueY, readyPod, errors.New(eventMsg)
	}
	if m.Status.DatafilesPatched != "true" {
		eventReason := "Datapatch Pending"
		eventMsg := "datapatch execution pending"
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
	}
	available = append(available, readyPod)
	podNamesFinal := dbcommons.GetPodNames(available)
	r.Log.Info("Final "+m.Name+" Pods After Deleting (or) Adding Extra Pods ( Including The Ready Pod ) ", "Pod Names", podNamesFinal)
	r.Log.Info(m.Name+" Replicas Available", "Count", len(podNamesFinal))
	r.Log.Info(m.Name+" Replicas Required", "Count", m.Spec.Replicas)

	eventReason := "Database Ready"
	eventMsg := "database open on pod " + readyPod.Name + " scheduled on node " + readyPod.Status.HostIP
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	m.Status.DatafilesCreated = "true"

	// DB is ready, fetch and update other info
	out, err := dbcommons.GetDatabaseRole(readyPod, r, r.Config, ctx, req, m.Spec.Edition)
	if err == nil {
		m.Status.Role = strings.ToUpper(out)
	}
	version, out, err := dbcommons.GetDatabaseVersion(readyPod, r, r.Config, ctx, req, m.Spec.Edition)
	if err == nil {
		if !strings.Contains(out, "ORA-") && m.Status.DatafilesPatched != "true" {
			m.Status.ReleaseUpdate = version
		}
	}

	if m.Spec.Edition == "express" {
		//Configure OEM Express Listener
		out, err = dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false,
			"bash", "-c", fmt.Sprintf("echo -e  \"%s\"  | su -p oracle -c \"sqlplus -s / as sysdba\" ", dbcommons.ConfigureOEMSQL))
		if err != nil {
			r.Log.Error(err, err.Error())
			return requeueY, readyPod, err
		}
		r.Log.Info("ConfigureOEMSQL output")
		r.Log.Info(out)
	}

	return requeueN, readyPod, nil

}

//#############################################################################
//    Function for deleting the Oracle Wallet
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) deleteWallet(m *dbapi.SingleInstanceDatabase, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Wallet not supported for XE Database
	if m.Spec.Edition == "express" {
		return requeueN, nil
	}

	// Deleting the secret and then deleting the wallet
	// If the secret is not found it means that the secret and wallet both are deleted, hence no need to requeue
	if !m.Spec.AdminPassword.KeepSecret {
		r.Log.Info("Querying the database secret ...")
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: m.Spec.AdminPassword.SecretName, Namespace: m.Namespace}, secret)
		if err == nil {
			err := r.Delete(ctx, secret)
			if err == nil {
				r.Log.Info("Deleted the secret : " + m.Spec.AdminPassword.SecretName)
			}
		}
	}

	// Getting the ready pod for the database
	readyPod, _, _, _, err := dbcommons.FindPods(r, m.Spec.Image.Version,
		m.Spec.Image.PullFrom, m.Name, m.Namespace, ctx, req)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, err
	}

	// Deleting the wallet
	_, err = dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "",
		ctx, req, false, "bash", "-c", dbcommons.WalletDeleteCMD)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, nil
	}
	r.Log.Info("Wallet Deleted !!")
	return requeueN, nil
}

//#############################################################################
//   Execute Datapatch
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) runDatapatch(m *dbapi.SingleInstanceDatabase,
	readyPod corev1.Pod, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Datapatch not supported for XE Database
	if m.Spec.Edition == "express" {
		return requeueN, nil
	}

	m.Status.Status = dbcommons.StatusPatching
	r.Status().Update(ctx, m)
	eventReason := "Datapatch Executing"
	eventMsg := "datapatch begin execution"
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	//RUN DATAPATCH
	out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "",
		ctx, req, false, "bash", "-c", dbcommons.RunDatapatchCMD)
	if err != nil {
		r.Log.Error(err, err.Error())
		return requeueY, err
	}
	r.Log.Info("Datapatch output")
	r.Log.Info(out)

	// Get Sqlpatch Description
	out, err = dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
		fmt.Sprintf("echo -e  \"%s\"  | sqlplus -s / as sysdba ", dbcommons.GetSqlpatchDescriptionSQL))
	if err == nil {
		r.Log.Info("GetSqlpatchDescriptionSQL Output")
		r.Log.Info(out)
		SqlpatchDescriptions, _ := dbcommons.StringToLines(out)
		if len(SqlpatchDescriptions) > 0 {
			m.Status.ReleaseUpdate = SqlpatchDescriptions[0]
		}
	}

	eventReason = "Datapatch Done"
	if strings.Contains(out, "Datapatch execution has failed.") {
		eventMsg = "datapatch execution failed"
		r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)
		return requeueN, errors.New(eventMsg)
	}

	m.Status.DatafilesPatched = "true"
	status, versionFrom, versionTo, _ := dbcommons.GetSqlpatchStatus(r, r.Config, readyPod, ctx, req)
	eventMsg = "data files patched from " + versionFrom + " to " + versionTo + " : " + status
	r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)

	return requeueN, nil
}

//#############################################################################
//    Update Init Parameters
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) updateInitParameters(m *dbapi.SingleInstanceDatabase,
	readyPod corev1.Pod, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("updateInitParameters", req.NamespacedName)

	if m.Status.InitParams == m.Spec.InitParams {
		return requeueN, nil
	}

	out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "",
		ctx, req, false, "bash", "-c", fmt.Sprintf(dbcommons.AlterSgaPgaCpuCMD, m.Spec.InitParams.SgaTarget,
			m.Spec.InitParams.PgaAggregateTarget, m.Spec.InitParams.CpuCount, dbcommons.GetSqlClient(m.Spec.Edition)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, err
	}
	log.Info("AlterSgaPgaCpuCMD Output:" + out)

	if m.Status.InitParams.Processes != m.Spec.InitParams.Processes {
		// Altering 'Processes' needs database to be restarted
		out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "",
			ctx, req, false, "bash", "-c", fmt.Sprintf(dbcommons.AlterProcessesCMD, m.Spec.InitParams.Processes, dbcommons.GetSqlClient(m.Spec.Edition),
				dbcommons.GetSqlClient(m.Spec.Edition)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		log.Info("AlterProcessesCMD Output:" + out)
	}

	out, err = dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "",
		ctx, req, false, "bash", "-c", fmt.Sprintf(dbcommons.GetInitParamsSQL, dbcommons.GetSqlClient(m.Spec.Edition)))
	if err != nil {
		log.Error(err, err.Error())
		return requeueY, err
	}
	log.Info("GetInitParamsSQL Output:" + out)

	m.Status.InitParams = m.Spec.InitParams
	return requeueN, nil
}

//#############################################################################
//    Update DB config params like FLASHBACK , FORCELOGGING , ARCHIVELOG
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) updateDBConfig(m *dbapi.SingleInstanceDatabase,
	readyPod corev1.Pod, ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	log := r.Log.WithValues("updateDBConfig", req.NamespacedName)

	m.Status.Status = dbcommons.StatusUpdating
	r.Status().Update(ctx, m)
	var forceLoggingStatus bool
	var flashBackStatus bool
	var archiveLogStatus bool
	var changeArchiveLog bool // True if switching ArchiveLog mode change is needed

	//#################################################################################################
	//                  CHECK FLASHBACK , ARCHIVELOG , FORCELOGGING
	//#################################################################################################

	flashBackStatus, archiveLogStatus, forceLoggingStatus, result := dbcommons.CheckDBConfig(readyPod, r, r.Config, ctx, req, m.Spec.Edition)
	if result.Requeue {
		return result, nil
	}
	m.Status.ArchiveLog = strconv.FormatBool(archiveLogStatus)
	m.Status.ForceLogging = strconv.FormatBool(forceLoggingStatus)
	m.Status.FlashBack = strconv.FormatBool(flashBackStatus)

	log.Info("Flashback", "Status :", flashBackStatus)
	log.Info("ArchiveLog", "Status :", archiveLogStatus)
	log.Info("ForceLog", "Status :", forceLoggingStatus)

	//#################################################################################################
	//                  TURNING FLASHBACK , ARCHIVELOG , FORCELOGGING TO TRUE
	//#################################################################################################

	if m.Spec.ArchiveLog && !archiveLogStatus {

		out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "",
			ctx, req, false, "bash", "-c", dbcommons.CreateDBRecoveryDestCMD)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		log.Info("CreateDbRecoveryDest Output")
		log.Info(out)

		out, err = dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | %s", dbcommons.SetDBRecoveryDestSQL, dbcommons.GetSqlClient(m.Spec.Edition)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		log.Info("SetDbRecoveryDest Output")
		log.Info(out)

		out, err = dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf(dbcommons.ArchiveLogTrueCMD, dbcommons.GetSqlClient(m.Spec.Edition)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		log.Info("ArchiveLogTrue Output")
		log.Info(out)

	}

	if m.Spec.ForceLogging && !forceLoggingStatus {
		out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | %s", dbcommons.ForceLoggingTrueSQL, dbcommons.GetSqlClient(m.Spec.Edition)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		log.Info("ForceLoggingTrue Output")
		log.Info(out)

	}
	if m.Spec.FlashBack && !flashBackStatus {
		_, archiveLogStatus, _, result := dbcommons.CheckDBConfig(readyPod, r, r.Config, ctx, req, m.Spec.Edition)
		if result.Requeue {
			return result, nil
		}
		if archiveLogStatus {
			out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
				fmt.Sprintf("echo -e  \"%s\"  | %s", dbcommons.FlashBackTrueSQL, dbcommons.GetSqlClient(m.Spec.Edition)))
			if err != nil {
				log.Error(err, err.Error())
				return requeueY, err
			}
			log.Info("FlashBackTrue Output")
			log.Info(out)

		} else {
			// Occurs when flashback is attermpted to be turned on without turning on archiving first
			eventReason := "Waiting"
			eventMsg := "enable ArchiveLog to turn ON Flashback"
			log.Info(eventMsg)
			r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)

			changeArchiveLog = true
		}
	}

	//#################################################################################################
	//                  TURNING FLASHBACK , ARCHIVELOG , FORCELOGGING TO FALSE
	//#################################################################################################

	if !m.Spec.FlashBack && flashBackStatus {
		out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | %s", dbcommons.FlashBackFalseSQL, dbcommons.GetSqlClient(m.Spec.Edition)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		log.Info("FlashBackFalse Output")
		log.Info(out)
	}
	if !m.Spec.ArchiveLog && archiveLogStatus {
		flashBackStatus, _, _, result := dbcommons.CheckDBConfig(readyPod, r, r.Config, ctx, req, m.Spec.Edition)
		if result.Requeue {
			return result, nil
		}
		if !flashBackStatus {

			out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
				fmt.Sprintf(dbcommons.ArchiveLogFalseCMD, dbcommons.GetSqlClient(m.Spec.Edition)))
			if err != nil {
				log.Error(err, err.Error())
				return requeueY, err
			}
			log.Info("ArchiveLogFalse Output")
			log.Info(out)

		} else {
			// Occurs when archiving is attermpted to be turned off without turning off flashback first
			eventReason := "Waiting"
			eventMsg := "turn OFF Flashback to disable ArchiveLog"
			log.Info(eventMsg)
			r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)

			changeArchiveLog = true
		}
	}
	if !m.Spec.ForceLogging && forceLoggingStatus {
		out, err := dbcommons.ExecCommand(r, r.Config, readyPod.Name, readyPod.Namespace, "", ctx, req, false, "bash", "-c",
			fmt.Sprintf("echo -e  \"%s\"  | %s", dbcommons.ForceLoggingFalseSQL, dbcommons.GetSqlClient(m.Spec.Edition)))
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		log.Info("ForceLoggingFalse Output")
		log.Info(out)
	}

	//#################################################################################################
	//                  CHECK FLASHBACK , ARCHIVELOG , FORCELOGGING
	//#################################################################################################

	flashBackStatus, archiveLogStatus, forceLoggingStatus, result = dbcommons.CheckDBConfig(readyPod, r, r.Config, ctx, req, m.Spec.Edition)
	if result.Requeue {
		return result, nil
	}

	log.Info("Flashback", "Status :", flashBackStatus)
	log.Info("ArchiveLog", "Status :", archiveLogStatus)
	log.Info("ForceLog", "Status :", forceLoggingStatus)

	m.Status.ArchiveLog = strconv.FormatBool(archiveLogStatus)
	m.Status.ForceLogging = strconv.FormatBool(forceLoggingStatus)

	// If Flashback has turned from OFF to ON in this reconcile ,
	// Needs to restart the Non Ready Pods ( Delete old ones and create new ones )
	if m.Status.FlashBack == strconv.FormatBool(false) && flashBackStatus {

		// call FindPods() to fetch pods all version/images of the same SIDB kind
		readyPod, replicasFound, available, _, err := dbcommons.FindPods(r, "", "", m.Name, m.Namespace, ctx, req)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
		// delete non ready Pods as flashback needs restart of pods
		_, err = r.deletePods(ctx, req, m, available, readyPod, replicasFound, 1)
		return requeueY, err
	}

	m.Status.FlashBack = strconv.FormatBool(flashBackStatus)

	if !changeArchiveLog && (flashBackStatus != m.Spec.FlashBack ||
		archiveLogStatus != m.Spec.ArchiveLog || forceLoggingStatus != m.Spec.ForceLogging) {
		return requeueY, nil
	}
	return requeueN, nil
}

//#############################################################################
//   Manage Finalizer to cleanup before deletion of SingleInstanceDatabase
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) manageSingleInstanceDatabaseDeletion(req ctrl.Request, ctx context.Context,
	m *dbapi.SingleInstanceDatabase) (ctrl.Result, error) {
	log := r.Log.WithValues("manageSingleInstanceDatabaseDeletion", req.NamespacedName)

	// Check if the SingleInstanceDatabase instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isSingleInstanceDatabaseMarkedToBeDeleted := m.GetDeletionTimestamp() != nil
	if isSingleInstanceDatabaseMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(m, singleInstanceDatabaseFinalizer) {
			// Run finalization logic for singleInstanceDatabaseFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			result, err := r.cleanupSingleInstanceDatabase(req, ctx, m)
			if result.Requeue {
				return result, err
			}

			// Remove SingleInstanceDatabaseFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			controllerutil.RemoveFinalizer(m, singleInstanceDatabaseFinalizer)
			err = r.Update(ctx, m)
			if err != nil {
				log.Error(err, err.Error())
				return requeueY, err
			}
			log.Info("Successfully Removed SingleInstanceDatabase Finalizer")
		}
		return requeueY, errors.New("deletion pending")
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(m, singleInstanceDatabaseFinalizer) {
		controllerutil.AddFinalizer(m, singleInstanceDatabaseFinalizer)
		err := r.Update(ctx, m)
		if err != nil {
			log.Error(err, err.Error())
			return requeueY, err
		}
	}
	return requeueN, nil
}

//#############################################################################
//   Finalization logic for singleInstanceDatabaseFinalizer
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) cleanupSingleInstanceDatabase(req ctrl.Request, ctx context.Context,
	m *dbapi.SingleInstanceDatabase) (ctrl.Result, error) {
	log := r.Log.WithValues("cleanupSingleInstanceDatabase", req.NamespacedName)
	// Cleanup steps that the operator needs to do before the CR can be deleted.

	if m.Status.OrdsReference != "" {
		eventReason := "Cannot cleanup"
		eventMsg := "uninstall ORDS to clean this SIDB"
		r.Recorder.Eventf(m, corev1.EventTypeNormal, eventReason, eventMsg)
		m.Status.Status = dbcommons.StatusError
		return requeueY, nil
	}

	// call deletePods() with zero pods in avaiable and nil readyPod to delete all pods
	result, err := r.deletePods(ctx, req, m, []corev1.Pod{}, corev1.Pod{}, 0, 0)
	if result.Requeue {
		return result, err
	}

	for {
		podList := &corev1.PodList{}
		listOpts := []client.ListOption{client.InNamespace(req.Namespace), client.MatchingLabels(dbcommons.GetLabelsForController("", req.Name))}

		if err := r.List(ctx, podList, listOpts...); err != nil {
			log.Error(err, "Failed to list pods of "+req.Name, "Namespace", req.Namespace)
			return requeueY, err
		}
		if len(podList.Items) == 0 {
			break
		}
		var podNames = ""
		for _, pod := range podList.Items {
			podNames += pod.Name + " "
		}
		eventReason := "Waiting"
		eventMsg := "waiting for " + req.Name + " database pods ( " + podNames + " ) to terminate"
		r.Recorder.Eventf(m, corev1.EventTypeWarning, eventReason, eventMsg)
		r.Log.Info(eventMsg)
		time.Sleep(15 * time.Second)
	}

	log.Info("Successfully cleaned up SingleInstanceDatabase")
	return requeueN, nil
}

//#############################################################################
//    SetupWithManager sets up the controller with the Manager
//#############################################################################
func (r *SingleInstanceDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbapi.SingleInstanceDatabase{}).
		Owns(&corev1.Pod{}). //Watch for deleted pods of SingleInstanceDatabase Owner
		WithEventFilter(dbcommons.ResourceEventHandler()).
		WithOptions(controller.Options{MaxConcurrentReconciles: 100}). //ReconcileHandler is never invoked concurrently with the same object.
		Complete(r)
}
