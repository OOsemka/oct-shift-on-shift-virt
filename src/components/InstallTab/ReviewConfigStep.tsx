import React, { FC, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  CodeBlock,
  CodeBlockCode,
  Form,
  FormGroup,
  Spinner,
  TextArea,
} from '@patternfly/react-core';
import { ClusterConfig } from './types';
import { previewConfig } from '../../utils/api';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
}

interface ConfigPreview {
  installConfig: string;
  agentConfig: string;
}

const ReviewConfigStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);
  const [preview, setPreview] = useState<ConfigPreview>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();

  useEffect(() => {
    setLoading(true);
    onChange({ installConfigOverrides: '', agentConfigOverrides: '' });
    previewConfig(config)
      .then((data: unknown) => {
        const p = data as ConfigPreview;
        setPreview(p);
        onChange({ installConfigOverrides: p.installConfig, agentConfigOverrides: p.agentConfig });
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, [config.clusterName, config.baseDomain, config.network.cudnSubnet, config.topology]);

  if (loading) return <Spinner size="lg" />;
  if (error) return <Alert variant="danger" isInline title={t('Error')}>{error}</Alert>;

  return (
    <Form className="sos-form">
      <FormGroup label="install-config.yaml" fieldId="install-config">
        <TextArea
          id="install-config"
          value={config.installConfigOverrides || preview?.installConfig || ''}
          onChange={(_e, val) => onChange({ installConfigOverrides: val })}
          rows={20}
          resizeOrientation="vertical"
          style={{ fontFamily: 'monospace', fontSize: '13px' }}
        />
      </FormGroup>

      <FormGroup label="agent-config.yaml" fieldId="agent-config">
        <TextArea
          id="agent-config"
          value={config.agentConfigOverrides || preview?.agentConfig || ''}
          onChange={(_e, val) => onChange({ agentConfigOverrides: val })}
          rows={20}
          resizeOrientation="vertical"
          style={{ fontFamily: 'monospace', fontSize: '13px' }}
        />
      </FormGroup>

      {config.clusterName && config.baseDomain && config.externalAccess.apiIP && (
        <Alert variant="info" isInline title={t('DNS Records')}>
          <CodeBlock>
            <CodeBlockCode>
              {`api.${config.clusterName}.${config.baseDomain}    A    ${config.externalAccess.apiIP}\n*.apps.${config.clusterName}.${config.baseDomain}  A    ${config.externalAccess.ingressIP}`}
            </CodeBlockCode>
          </CodeBlock>
        </Alert>
      )}
    </Form>
  );
};

export default ReviewConfigStep;
