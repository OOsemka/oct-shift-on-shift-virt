import React, { FC, useState } from 'react';
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

  const handleDeploy = async () => {
    setSubmitting(true);
    setError(undefined);
    try {
      const subnetMatch = config.network.cudnSubnet.match(/^(\d+\.\d+\.\d+)\.\d+\/\d+$/);
      const deployConfig = { ...config };
      if (subnetMatch) {
        deployConfig.network = {
          ...config.network,
          apiVIP: `${subnetMatch[1]}.10`,
          ingressVIP: `${subnetMatch[1]}.11`,
        };
      }
      await createCluster(deployConfig);
      navigate(`/community-tools/compute/shift-on-shift-virt/cluster/${encodeURIComponent(config.clusterName)}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
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
        <WizardStep name={t('Network')} id="network">
          <NetworkStep config={config} onChange={onNetworkChange} />
        </WizardStep>
        <WizardStep name={t('External Access')} id="external-access">
          <ExternalAccessStep config={config} onChange={onExternalAccessChange} />
        </WizardStep>
        <WizardStep name={t('Storage')} id="storage">
          <StorageStep config={config} onChange={onChange} />
        </WizardStep>
        <WizardStep name={t('Credentials')} id="credentials">
          <CredentialsStep config={config} onChange={onChange} />
        </WizardStep>
        <WizardStep
          name={t('Review & Edit Configs')}
          id="review"
          footer={{
            nextButtonText: t('Deploy Cluster'),
            onNext: handleDeploy,
            isNextDisabled: submitting,
          }}
        >
          <ReviewConfigStep config={config} onChange={onChange} />
        </WizardStep>
      </Wizard>
    </>
  );
};

export default CreateClusterWizard;
