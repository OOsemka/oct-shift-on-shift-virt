package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type labDefaults struct {
	PullSecretSet     bool            `json:"pullSecretSet"`
	PullSecretProblem string          `json:"pullSecretProblem,omitempty"`
	SSHKey            string          `json:"sshPublicKey,omitempty"`
	SSHKeyFingerprint string          `json:"sshKeyFingerprint,omitempty"`
	SSHKeyProblem     string          `json:"sshKeyProblem,omitempty"`
	Settings          crd.LabSettings `json:"settings"`
	SettingsProblem   string          `json:"settingsProblem,omitempty"`
	pullSecretValid   bool
}

// loadLabDefaults reads the lab defaults Secret. The pull secret itself is
// only checked, never returned.
func (s *Server) loadLabDefaults(ctx context.Context) labDefaults {
	var d labDefaults
	sec, err := s.client.Clientset.CoreV1().Secrets(clusterNamespace).Get(ctx, crd.LabDefaultsSecret, metav1.GetOptions{})
	if err != nil {
		return d
	}
	if pull := strings.TrimSpace(string(sec.Data[crd.LabDefaultsPullSecret])); pull != "" {
		d.PullSecretSet = true
		if err := crd.ValidatePullSecret(pull); err != nil {
			d.PullSecretProblem = err.Error()
		} else {
			d.pullSecretValid = true
		}
	}
	if key := strings.TrimSpace(string(sec.Data[crd.LabDefaultsSSHKey])); key != "" {
		if err := crd.ValidateSSHPublicKey(key); err != nil {
			d.SSHKeyProblem = err.Error()
		} else {
			d.SSHKey = key
			d.SSHKeyFingerprint = crd.SSHKeyFingerprint(key)
		}
	}
	settings, err := crd.ParseLabSettings(sec.Data[crd.LabDefaultsSettingsKey])
	if err != nil {
		d.SettingsProblem = err.Error()
	}
	d.Settings = settings.WithDefaults()
	return d
}

func (s *Server) getLabDefaults(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.loadLabDefaults(r.Context()))
}

type quickPlan struct {
	Spec     crd.NestedClusterSpec `json:"spec"`
	Problems []string              `json:"problems"`
}

// planQuickCluster builds the quick cluster for name ("" = suggest one).
func (s *Server) planQuickCluster(ctx context.Context, name string) (quickPlan, error) {
	clusters, err := s.clusterSpecs(ctx)
	if err != nil {
		return quickPlan{}, err
	}
	if name = strings.TrimSpace(name); name == "" {
		names := make([]string, 0, len(clusters))
		for _, c := range clusters {
			names = append(names, c.ClusterName)
		}
		name = crd.SuggestQuickName(names)
	}
	d := s.loadLabDefaults(ctx)
	var pool *crd.IPPool
	if d.Settings.IPPool != "" {
		if p, err := s.getIPPool(ctx, d.Settings.IPPool); err == nil {
			pool = p
		}
	}
	// The internal network must stay clear of the host nodes. If they cannot
	// be read the usual default is planned and createNestedCluster decides.
	host, _, err := s.hostNodeAddresses(ctx)
	if err != nil {
		s.logger.Info("cannot list host nodes for the quick cluster plan", "error", err.Error())
	}
	spec, problems := crd.BuildQuickSpec(name, d.Settings, d.pullSecretValid, pool, crd.FreePoolAddresses(pool, clusters), host)
	for _, c := range clusters {
		if c.ClusterName == name {
			problems = append(problems, "a cluster named "+name+" already exists")
		}
	}
	if problems == nil {
		problems = []string{}
	}
	return quickPlan{Spec: spec, Problems: problems}, nil
}

func (s *Server) previewQuickCluster(w http.ResponseWriter, r *http.Request) {
	plan, err := s.planQuickCluster(r.Context(), r.URL.Query().Get("name"))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, plan)
}

// createQuickCluster creates a compact cluster from the lab defaults. Only the
// name comes from the request; everything else is decided here, so a client
// cannot slip other settings in through this endpoint.
func (s *Server) createQuickCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClusterName string `json:"clusterName"`
		crd.QuickOverrides
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	plan, err := s.planQuickCluster(r.Context(), req.ClusterName)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only the SSH key and the desktop choice may be overridden.
	plan.Problems = append(plan.Problems, crd.ApplyQuickOverrides(&plan.Spec, req.QuickOverrides)...)
	if len(plan.Problems) > 0 {
		s.writeError(w, http.StatusBadRequest, "cannot create a quick cluster: "+strings.Join(plan.Problems, "; "))
		return
	}
	created, code, err := s.createNestedCluster(r.Context(), plan.Spec)
	if err != nil {
		s.writeError(w, code, err.Error())
		return
	}
	s.logger.Info("quick cluster created", "cluster", plan.Spec.ClusterName,
		"api", plan.Spec.ExternalAccess.APIIP, "ingress", plan.Spec.ExternalAccess.IngressIP, "pool", plan.Spec.ExternalAccess.IPPool)
	s.writeJSON(w, http.StatusCreated, created.Object)
}
