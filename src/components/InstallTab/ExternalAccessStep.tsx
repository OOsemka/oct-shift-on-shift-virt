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
  NumberInput,
  Spinner,
  TextInput,
} from '@patternfly/react-core';
import { K8sResourceCommon, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { ClusterConfig } from './types';
import { externalAccessProblems, NetworkField } from '../../utils/network-check';

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

  // Show "required" only after the user has typed something somewhere on this
  // step; the wizard's Next stays disabled either way.
  const problems = externalAccessProblems(config);
  const touched = Boolean(ea.nadName || ea.apiIP || ea.ingressIP);
  const errorFor = (field: NetworkField) => {
    const p = problems.find((x) => x.field === field);
    return p && (touched || field === 'prefixLength') ? t(p.key, p.values) : undefined;
  };
  const nadErr = errorFor('nadName');
  const apiErr = errorFor('apiIP');
  const ingressErr = errorFor('ingressIP');
  const prefixErr = errorFor('prefixLength');

  return (
    <Form className="sos-form">
      <Alert variant="info" isInline title={t('These addresses are not assigned automatically')}>
        {t('Pick the network (NAD) of a VLAN that people can reach, then type two free addresses on that VLAN: one for the API and one for Ingress. Do not use the internal VIPs from the Network step. Ask your network team for free addresses if you are unsure.')}
      </Alert>
      <FormGroup label={t('External network (NAD)')} isRequired fieldId="nad-name">
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
        {nadErr && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="error">{nadErr}</HelperTextItem>
            </HelperText>
          </FormHelperText>
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

      <FormGroup label={t('External API IP')} isRequired fieldId="api-ip">
        <TextInput
          id="api-ip"
          value={ea.apiIP}
          onChange={(_e, val) => onChange({ apiIP: val })}
          validated={apiErr ? 'error' : 'default'}
          placeholder="192.168.100.10"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={apiErr ? 'error' : 'default'}>
              {apiErr || t('Free address on the external VLAN for api.<cluster>.<domain>.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('External Ingress IP')} isRequired fieldId="ingress-ip">
        <TextInput
          id="ingress-ip"
          value={ea.ingressIP}
          onChange={(_e, val) => onChange({ ingressIP: val })}
          validated={ingressErr ? 'error' : 'default'}
          placeholder="192.168.100.11"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={ingressErr ? 'error' : 'default'}>
              {ingressErr || t('Another free address on the same VLAN, for *.apps.<cluster>.<domain>.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>
      <FormGroup label={t('VLAN prefix length')} isRequired fieldId="prefix-length">
        <NumberInput
          id="prefix-length"
          value={ea.prefixLength}
          min={8}
          max={30}
          onMinus={() => onChange({ prefixLength: Math.max(8, (ea.prefixLength || 24) - 1) })}
          onPlus={() => onChange({ prefixLength: Math.min(30, (ea.prefixLength || 24) + 1) })}
          onChange={(e) => {
            const n = parseInt((e.target as HTMLInputElement).value, 10);
            onChange({ prefixLength: isNaN(n) ? 0 : n });
          }}
          widthChars={4}
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={prefixErr ? 'error' : 'default'}>
              {prefixErr || t('Size of the external VLAN subnet, for example 24 for a /24 or 22 for a /22.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      {fqdn && ea.apiIP && ea.ingressIP && !apiErr && !ingressErr && (
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
