/*
 (c) Copyright [2021-2022] Micro Focus or one of its affiliates.
 Licensed under the Apache License, Version 2.0 (the "License");
 You may not use this file except in compliance with the License.
 You may obtain a copy of the License at

 http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package vdb

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	vapi "github.com/vertica/vertica-kubernetes/api/v1beta1"
	"github.com/vertica/vertica-kubernetes/pkg/atconf"
	"github.com/vertica/vertica-kubernetes/pkg/cmds"
	"github.com/vertica/vertica-kubernetes/pkg/controllers"
	"github.com/vertica/vertica-kubernetes/pkg/events"
	"github.com/vertica/vertica-kubernetes/pkg/httpconf"
	"github.com/vertica/vertica-kubernetes/pkg/names"
	"github.com/vertica/vertica-kubernetes/pkg/paths"
	"github.com/vertica/vertica-kubernetes/pkg/version"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// InstallReconciler will handle reconcile for install of vertica
type InstallReconciler struct {
	VRec     *VerticaDBReconciler
	Log      logr.Logger
	Vdb      *vapi.VerticaDB // Vdb is the CRD we are acting on.
	PRunner  cmds.PodRunner
	PFacts   *PodFacts
	ATWriter atconf.Writer
}

// MakeInstallReconciler will build and return the InstallReconciler object.
func MakeInstallReconciler(vdbrecon *VerticaDBReconciler, log logr.Logger,
	vdb *vapi.VerticaDB, prunner cmds.PodRunner, pfacts *PodFacts) controllers.ReconcileActor {
	return &InstallReconciler{
		VRec:     vdbrecon,
		Log:      log,
		Vdb:      vdb,
		PRunner:  prunner,
		PFacts:   pfacts,
		ATWriter: atconf.MakeFileWriter(log, vdb, prunner),
	}
}

// Reconcile will ensure Vertica is installed and running in the pods.
func (d *InstallReconciler) Reconcile(ctx context.Context, req *ctrl.Request) (ctrl.Result, error) {
	// no-op for ScheduleOnly init policy
	if d.Vdb.Spec.InitPolicy == vapi.CommunalInitPolicyScheduleOnly {
		return ctrl.Result{}, nil
	}

	// The reconcile loop works by collecting all of the facts about the running
	// pods. We then analyze those facts to determine a course of action to take.
	if err := d.PFacts.Collect(ctx, d.Vdb); err != nil {
		return ctrl.Result{}, err
	}
	return d.analyzeFacts(ctx)
}

// analyzeFacts will look at the collected facts and determine the course of action
func (d *InstallReconciler) analyzeFacts(ctx context.Context) (ctrl.Result, error) {
	// We can only proceed with install if all of the installed pods are
	// running.  This ensures we can properly sync admintools.conf.
	if ok, podNotRunning := d.PFacts.anyInstalledPodsNotRunning(); ok {
		d.Log.Info("At least one installed pod isn't running.  Aborting the install.", "pod", podNotRunning)
		return ctrl.Result{Requeue: true}, nil
	}
	if ok, podNotRunning := d.PFacts.anyUninstalledTransientPodsNotRunning(); ok {
		d.Log.Info("At least one transient pod isn't running and doesn't have an install", "pod", podNotRunning)
		return ctrl.Result{Requeue: true}, nil
	}

	fns := []func(context.Context) error{
		d.acceptEulaIfMissing,
		d.createConfigDirsIfNecessary,
		// This has to be after accepting the EULA.  re_ip will not succeed if
		// the EULA is not accepted and a re_ip can happen before coming to this
		// reconcile function.  So if the pod is rescheduled after adding
		// hosts to the config, we have to know that a re_ip will succeed.
		d.addHostsToATConf,
		d.generateHTTPCerts,
	}
	for _, fn := range fns {
		if err := fn(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// addHostsToATConf will add hosts for any pods that have not yet bootstrapped the config.
func (d *InstallReconciler) addHostsToATConf(ctx context.Context) error {
	pods, err := d.getInstallTargets(ctx)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return nil
	}

	installedPods := d.PFacts.findInstalledPods()
	ipsToInstall := []string{}
	for _, p := range pods {
		ipsToInstall = append(ipsToInstall, p.podIP)
	}
	installPod := types.NamespacedName{}
	if len(installedPods) != 0 {
		installPod, err = findATBasePod(d.Vdb, d.PFacts)
		if err != nil {
			return err
		}
	}

	atConfTempFile, err := d.ATWriter.AddHosts(ctx, installPod, ipsToInstall)
	if err != nil {
		return err
	}
	defer os.Remove(atConfTempFile)

	if d.VRec.OpCfg.DevMode {
		debugDumpAdmintoolsConfForPods(ctx, d.PRunner, installedPods)
	}
	if err := distributeAdmintoolsConf(ctx, d.Vdb, d.VRec, d.PFacts, d.PRunner, atConfTempFile); err != nil {
		return err
	}
	installedPods = append(installedPods, pods...)
	if d.VRec.OpCfg.DevMode {
		debugDumpAdmintoolsConfForPods(ctx, d.PRunner, installedPods)
	}

	// Invalidate the pod facts cache since its out of date due to the install
	d.PFacts.Invalidate()

	return d.createInstallIndicators(ctx, pods)
}

// acceptEulaIfMissing is a wrapper function that calls another function that
// accepts the end user license agreement.
func (d *InstallReconciler) acceptEulaIfMissing(ctx context.Context) error {
	return acceptEulaIfMissing(ctx, d.PFacts, d.PRunner)
}

// createConfigDirsIfNecessary will check that certain directories in /opt/vertica/config
// exists and are writable by dbadmin
func (d *InstallReconciler) createConfigDirsIfNecessary(ctx context.Context) error {
	for _, p := range d.PFacts.Detail {
		if err := d.createConfigDirsForPodIfNecessary(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// generateHTTPCerts will generate the necessary certs to be able to start and
// communicate with the Vertica's http server.
func (d *InstallReconciler) generateHTTPCerts(ctx context.Context) error {
	// Early out if the http service isn't enabled
	if !d.doHTTPInstall(true) {
		return nil
	}
	for _, p := range d.PFacts.Detail {
		if !p.isPodRunning {
			continue
		}
		if !p.httpTLSConfExists {
			frwt := httpconf.FileWriter{}
			secretName := names.GenNamespacedName(d.Vdb, d.Vdb.Spec.HTTPServerSecret)
			fname, err := frwt.GenConf(ctx, d.VRec.Client, secretName)
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("failed generating the %s file", paths.HTTPTLSConfFile))
			}
			_, _, err = d.PRunner.CopyToPod(ctx, p.name, names.ServerContainer, fname,
				fmt.Sprintf("%s/%s", paths.HTTPTLSConfDir, paths.HTTPTLSConfFile))
			_ = os.Remove(fname)
			if err != nil {
				return errors.Wrap(err, fmt.Sprintf("failed to copy %s to the pod %s", fname, p.name))
			}
		}
	}
	return nil
}

// getInstallTargets finds the list of hosts/pods that we need to initialize the config for
func (d *InstallReconciler) getInstallTargets(ctx context.Context) ([]*PodFact, error) {
	podList := make([]*PodFact, 0, len(d.PFacts.Detail))
	// We need to install pods in pod index order.  We do this because we can
	// determine if a pod has an installation by looking at the install count
	// for the subcluster.  For instance, if a subcluster of size 3 has no
	// installation, and pod-1 isn't running, we can only install pod-0.  Pod-2
	// needs to wait for the installation of pod-1.
	scMap := d.Vdb.GenSubclusterMap()
	for _, sc := range scMap {
		startPodIndex := int32(0)
		scStatus, ok := d.Vdb.FindSubclusterStatus(sc.Name)
		if ok {
			startPodIndex += scStatus.InstallCount
		}
		for i := startPodIndex; i < sc.Size; i++ {
			pn := names.GenPodName(d.Vdb, sc, i)
			v, ok := d.PFacts.Detail[pn]
			if !ok {
				break
			}
			if v.isInstalled || v.dbExists {
				continue
			}
			// To ensure we only install pods in pod-index order, we stop the
			// install target search when we find a pod isn't running.
			if !v.isPodRunning {
				break
			}
			podList = append(podList, v)

			if v.hasStaleAdmintoolsConf {
				if _, _, err := d.PRunner.ExecInPod(ctx, v.name, names.ServerContainer, d.genCmdRemoveOldConfig()...); err != nil {
					return podList, fmt.Errorf("failed to remove old admintools.conf: %w", err)
				}
			}
		}
	}
	return podList, nil
}

// createInstallIndicators will create the install indicator file for all pods passed in
func (d *InstallReconciler) createInstallIndicators(ctx context.Context, pods []*PodFact) error {
	for _, v := range pods {
		// Create the install indicator file. This is used to know that this
		// instance of the vdb has setup the config for this pod. The
		// /opt/vertica/config is backed by a PV, so it is possible that we
		// see state in there for a prior instance of the vdb. We use the
		// UID of the vdb to know the current instance.
		d.Log.Info("create installer indicator file", "Pod", v.name)
		cmd := d.genCmdCreateInstallIndicator(v)
		if stdout, _, err := d.PRunner.ExecInPod(ctx, v.name, names.ServerContainer, cmd...); err != nil {
			return fmt.Errorf("failed to create installer indicator with command '%s', output was '%s': %w", cmd, stdout, err)
		}
	}
	return nil
}

// genCmdCreateInstallIndicator generates the command to create the install indicator file
func (d *InstallReconciler) genCmdCreateInstallIndicator(pf *PodFact) []string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("grep -E '^node[0-9]{4} = %s,' %s", pf.podIP, paths.AdminToolsConf))
	sb.WriteString(" | head -1 | cut -d' ' -f1 | tee ")
	// The install indicator file has the UID of the vdb. This allows us to know
	// that we are working with a different life in the vdb is ever recreated.
	sb.WriteString(d.Vdb.GenInstallerIndicatorFileName())
	return []string{"bash", "-c", sb.String()}
}

// genCmdRemoveOldConfig generates the command to remove the old admintools.conf file
func (d *InstallReconciler) genCmdRemoveOldConfig() []string {
	return []string{
		"mv",
		paths.AdminToolsConf,
		fmt.Sprintf("%s.uid.%s", paths.AdminToolsConf, string(d.Vdb.UID)),
	}
}

// doHTTPInstall will return true if the installer should setup for the http server
func (d *InstallReconciler) doHTTPInstall(logEvent bool) bool {
	// Early out if the http service isn't enabled
	if !d.Vdb.IsHTTPServerEnabled() {
		return false
	}
	vinf, ok := version.MakeInfoFromVdb(d.Vdb)
	if !ok || vinf.IsOlder(version.HTTPServerMinVersion) {
		if logEvent {
			d.VRec.Eventf(d.Vdb, corev1.EventTypeWarning, events.HTTPServerNotSetup,
				"Skipping http server cert setup because the Vertica version doesn't have "+
					"support for it. A Vertica version of '%s' or newer is needed", version.HTTPServerMinVersion)
		}
		return false
	}
	return true
}

// genCreateConfigDirsScript will create a script to be run in a pod to create
// the necessary dirs for install. This will return an empty string if nothing
// needs to happen.
func (d *InstallReconciler) genCreateConfigDirsScript(p *PodFact) string {
	var sb strings.Builder
	sb.WriteString("set -o errexit\n")
	numCmds := 0
	if p.configLogrotateExists && !p.configLogrotateWritable {
		// We enforce this in the docker entrypoint of the container too.  But
		// we have this here for backwards compatibility for the 11.0 image.
		// The 10.1.1 image doesn't even have logrotate, which is why we
		// first check if the directory exists.
		sb.WriteString(fmt.Sprintf("sudo chown -R dbadmin:verticadba %s\n", paths.ConfigLogrotatePath))
		numCmds++
	}

	if !p.configShareExists {
		sb.WriteString(fmt.Sprintf("mkdir %s\n", paths.ConfigSharePath))
		numCmds++
	}

	if !d.doHTTPInstall(false) && !p.httpTLSConfExists {
		sb.WriteString(fmt.Sprintf("mkdir -p %s\n", paths.HTTPTLSConfDir))
		numCmds++
	}

	if numCmds == 0 {
		return ""
	}
	return sb.String()
}

// createConfigDirsForPodIfNecesssary will setup the config dirs for a single pod.
func (d *InstallReconciler) createConfigDirsForPodIfNecessary(ctx context.Context, p *PodFact) error {
	if !p.isPodRunning {
		return nil
	}
	tmp, err := ioutil.TempFile("", "create-config-dirs.sh.")
	if err != nil {
		return err
	}
	defer tmp.Close()
	defer os.Remove(tmp.Name())

	script := d.genCreateConfigDirsScript(p)
	if script == "" {
		return nil
	}
	_, err = tmp.WriteString(script)
	if err != nil {
		return err
	}
	tmp.Close()

	// Copy the script into the pod and execute it
	_, _, err = d.PRunner.CopyToPod(ctx, p.name, names.ServerContainer, tmp.Name(), paths.CreateConfigDirsScript,
		"bash", paths.CreateConfigDirsScript)
	if err != nil {
		return errors.Wrap(err, "failed to copy and execute the config dirs script")
	}
	return nil
}
