import React, { FC, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom-v5-compat';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Wizard,
  WizardStep,
} from '@patternfly/react-core';
import { ClusterConfig, DEFAULT_CLUSTER_CONFIG } from './types';
import ClusterBasicsStep from './ClusterBasicsStep';
import TopologyStep from './TopologyStep';
import VMSizingStep from './VMSizingStep';
import NodeNamingStep from './NodeNamingStep';
import NetworkStep from './NetworkStep';
import ExternalAccessStep from './ExternalAccessStep';
import StorageStep from './StorageStep';
import CredentialsStep from './CredentialsStep';
import ReviewConfigStep from './ReviewConfigStep';
import { createCluster } from '../../utils/api';
import { externalAccessProblems, internalVIPs, networkProblems } from '../../utils/network-check';
import { fetchLabDefaults, LabDefaults } from '../../utils/labdefaults';

const I18N = 'plugin__oct-shift-on-shift-virt';

const CreateClusterWizard: FC = () => {
  const { t } = useTranslation(I18N);
  const navigate = useNavigate();
  const [config, setConfig] = useState<ClusterConfig>({ ...DEFAULT_CLUSTER_CONFIG });
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string>();

  const onChange = (patch: Partial<ClusterConfig>) => {
    setConfig((prev) => ({ ...prev, ...patch }));
  };

  const onNetworkChange = (patch: Partial<ClusterConfig['network']>) => {
    setConfig((prev) => ({ ...prev, network: { ...prev.network, ...patch } }));
  };

  const onExternalAccessChange = (patch: Partial<ClusterConfig['externalAccess']>) => {
    setConfig((prev) => ({ ...prev, externalAccess: { ...prev.externalAccess, ...patch } }));
  };

  const [labDefaults, setLabDefaults] = useState<LabDefaults | null>(null);
  useEffect(() => {
    fetchLabDefaults().then(setLabDefaults).catch(() => setLabDefaults(null));
  }, []);

  const netProblems = networkProblems(config.network);
  const eaProblems = externalAccessProblems(config);
  // Without a pull secret here or in the lab defaults the install would only
  // fail later, at CreateSecrets.
  const credentialProblems: { field: string; key: string; values?: Record<string, string> }[] =
    !config.pullSecret.trim() && !labDefaults?.pullSecretSet
      ? [{ field: 'pullSecret', key: 'Paste a pull secret on the Credentials step, or store one in the Lab Defaults tab.' }]
      : [];
  const blocking = [...netProblems, ...eaProblems, ...credentialProblems];

  const handleDeploy = async () => {
    if (blocking.length > 0) return;
    setSubmitting(true);
    setError(undefined);
    try {
      // The internal VIPs always follow the internal network (they used to be
      // set only when the subnet matched a pattern, keeping stale defaults otherwise).
      const vips = internalVIPs(config.network.cudnSubnet);
      const deployConfig = {
        ...config,
        network: { ...config.network, ...(vips || {}) },
        externalAccess: {
          ...config.externalAccess,
          nadName: config.externalAccess.nadName.trim(),
          apiIP: config.externalAccess.apiIP.trim(),
          ingressIP: config.externalAccess.ingressIP.trim(),
        },
      };
      await createCluster(deployConfig);
      navigate(`/community-tools/compute/shift-on-shift-virt/cluster/${encodeURIComponent(config.clusterName)}`);
    } catch (e) {
      // The orchestrator explains refusals in {"error": "..."}; show that text.
      const body = (e as { json?: { error?: string; message?: string } })?.json;
      setError(body?.error || body?.message || (e instanceof Error ? e.message : String(e)));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <>
      {error && (
        <Alert variant="danger" isInline title={t('Error')} className="sos-wizard-error">
          {error}
        </Alert>
      )}
      <Wizard
        onClose={() => setConfig({ ...DEFAULT_CLUSTER_CONFIG })}
      >
        <WizardStep name={t('Cluster Basics')} id="basics">
          <ClusterBasicsStep config={config} onChange={onChange} />
        </WizardStep>
        <WizardStep name={t('Topology')} id="topology">
          <TopologyStep config={config} onChange={onChange} />
        </WizardStep>
        <WizardStep name={t('VM Sizing')} id="vm-sizing">
          <VMSizingStep config={config} onChange={onChange} />
        </WizardStep>
        <WizardStep name={t('Node Naming')} id="node-naming">
          <NodeNamingStep config={config} onChange={onChange} />
        </WizardStep>
        <WizardStep name={t('Network')} id="network" footer={{ isNextDisabled: netProblems.length > 0 }}>
          <NetworkStep config={config} onChange={onNetworkChange} />
        </WizardStep>
        <WizardStep
          name={t('External Access')}
          id="external-access"
          footer={{ isNextDisabled: eaProblems.length > 0 }}
        >
          <ExternalAccessStep config={config} onChange={onExternalAccessChange} />
        </WizardStep>
        <WizardStep name={t('Storage')} id="storage">
          <StorageStep config={config} onChange={onChange} />
        </WizardStep>
        <WizardStep name={t('Credentials')} id="credentials">
          <CredentialsStep config={config} onChange={onChange} labDefaults={labDefaults} />
        </WizardStep>
        <WizardStep
          name={t('Review & Edit Configs')}
          id="review"
          footer={{
            nextButtonText: t('Deploy Cluster'),
            onNext: handleDeploy,
            isNextDisabled: submitting || blocking.length > 0,
          }}
        >
          {blocking.length > 0 && (
            <Alert
              variant="danger"
              isInline
              title={t('Fix the network settings before deploying')}
              className="sos-wizard-error"
            >
              <ul>
                {blocking.map((p) => (
                  <li key={`${p.field}-${p.key}`}>{t(p.key, p.values)}</li>
                ))}
              </ul>
            </Alert>
          )}
          <ReviewConfigStep config={config} onChange={onChange} />
        </WizardStep>
      </Wizard>
    </>
  );
};

export default CreateClusterWizard;
