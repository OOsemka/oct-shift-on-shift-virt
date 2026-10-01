import React, { FC, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  ClipboardCopy,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Spinner,
  TextInput,
} from '@patternfly/react-core';
import { K8sResourceCommon, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { ClusterConfig } from './types';
import { validateIP } from '../../utils/validation';

const I18N = 'plugin__oct-shift-on-shift-virt';

type NADKind = K8sResourceCommon & {
  metadata: { name: string; namespace: string };
};

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig['externalAccess']>) => void;
}

const ExternalAccessStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);
  const ea = config.externalAccess;
  const fqdn = config.clusterName && config.baseDomain
    ? `${config.clusterName}.${config.baseDomain}`
    : '';

  const userNs = config.namespace || 'default';

  const [nsNads, nsLoaded, nsError] = useK8sWatchResource<NADKind[]>({
    groupVersionKind: { group: 'k8s.cni.cncf.io', version: 'v1', kind: 'NetworkAttachmentDefinition' },
    namespace: userNs,
    isList: true,
  });

  const [defaultNads, defaultLoaded, defaultError] = useK8sWatchResource<NADKind[]>({
    groupVersionKind: { group: 'k8s.cni.cncf.io', version: 'v1', kind: 'NetworkAttachmentDefinition' },
    namespace: 'default',
    isList: true,
  });

  const nads = useMemo(() => {
    const nsItems = Array.isArray(nsNads) ? nsNads : [];
    const defaultItems = Array.isArray(defaultNads) ? defaultNads : [];
    const all = [...nsItems, ...defaultItems];
    const seen = new Set<string>();
    return all.filter((nad) => {
      const key = `${nad.metadata?.namespace}/${nad.metadata?.name}`;
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  }, [nsNads, defaultNads]);

  const nadsLoading = !nsLoaded || !defaultLoaded;
  const nadError = nsError || defaultError;

  const apiErr = ea.apiIP ? validateIP(ea.apiIP) : undefined;
  const ingressErr = ea.ingressIP ? validateIP(ea.ingressIP) : undefined;

  return (
    <Form className="sos-form">
      <FormGroup label={t('NAD Name')} isRequired fieldId="nad-name">
        {nadsLoading ? <Spinner size="md" /> : (
          <FormSelect
            id="nad-name"
            value={ea.nadName}
            onChange={(_e, val) => onChange({ nadName: val })}
          >
            <FormSelectOption key="" value="" label={t('Select a NetworkAttachmentDefinition')} isDisabled />
            {nads.map((nad) => (
              <FormSelectOption
                key={`${nad.metadata.namespace}/${nad.metadata.name}`}
                value={`${nad.metadata.namespace}/${nad.metadata.name}`}
                label={`${nad.metadata.name} (${nad.metadata.namespace})`}
              />
            ))}
          </FormSelect>
        )}
        {nadError && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="warning">{String(nadError)}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        )}
        <FormHelperText>
          <HelperText>
            <HelperTextItem>
              {t('Select a NAD connected to a physical VLAN for external access.')}
              {config.namespace && ` ${t('Showing NADs from "{{ns}}" and "default".', { ns: config.namespace })}`}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('API IP')} isRequired fieldId="api-ip">
        <TextInput
          id="api-ip"
          value={ea.apiIP}
          onChange={(_e, val) => onChange({ apiIP: val })}
          validated={apiErr ? 'error' : 'default'}
          placeholder="192.168.100.10"
        />
        {apiErr && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="error">{apiErr}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        )}
      </FormGroup>

      <FormGroup label={t('Ingress IP')} isRequired fieldId="ingress-ip">
        <TextInput
          id="ingress-ip"
          value={ea.ingressIP}
          onChange={(_e, val) => onChange({ ingressIP: val })}
          validated={ingressErr ? 'error' : 'default'}
          placeholder="192.168.100.11"
        />
        {ingressErr && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="error">{ingressErr}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        )}
      </FormGroup>

      {fqdn && ea.apiIP && ea.ingressIP && (
        <Alert variant="info" isInline title={t('DNS Records')} className="sos-dns-records">
          <p>{t('Add the following A records to your DNS:')}</p>
          <ClipboardCopy isReadOnly isCode variant="expansion" isExpanded>
            {`api.${fqdn}    A    ${ea.apiIP}\n*.apps.${fqdn}  A    ${ea.ingressIP}`}
          </ClipboardCopy>
        </Alert>
      )}
    </Form>
  );
};

export default ExternalAccessStep;
