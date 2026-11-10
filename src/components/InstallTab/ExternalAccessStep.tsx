import React, { FC, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Checkbox,
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
import { externalAccessProblems, NetworkField, suggestGateway } from '../../utils/network-check';
import { fetchIPPools, freeAddresses, IPPoolItem } from '../../utils/ippools';

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

  // IP pools: pick the network and free addresses from a list.
  const [pools, setPools] = useState<IPPoolItem[] | null>(null);
  useEffect(() => {
    fetchIPPools()
      .then(setPools)
      .catch(() => setPools([]));
  }, []);
  const pool = pools?.find((p) => p.name === ea.ipPool);
  const poolFree = pool ? freeAddresses(pool) : [];
  const choosePool = (name: string) => {
    if (!name) {
      onChange({ ipPool: '' });
      return;
    }
    const p = pools?.find((x) => x.name === name);
    if (!p) return;
    const free = freeAddresses(p);
    onChange({
      ipPool: p.name,
      nadName: p.nadName,
      prefixLength: p.prefixLength,
      gateway: p.gateway || '',
      apiIP: free[0] || '',
      ingressIP: free[1] || '',
    });
  };
  // Free addresses plus whatever is selected now, for the dropdowns.
  const poolChoices = (current: string) =>
    Array.from(new Set([...(current ? [current] : []), ...poolFree]));
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
  const gatewayErr = errorFor('gateway');

  return (
    <Form className="sos-form">
      <Alert variant="info" isInline title={t('These addresses are not assigned automatically')}>
        {t('Pick an IP pool, or pick the network (NAD) of a VLAN that people can reach and type two free addresses on that VLAN (one for the API, one for Ingress) and its gateway. The cluster\'s bastion VM takes these addresses and load-balances to the cluster. Do not use the internal VIPs from the Network step. Ask your network team for free addresses if you are unsure.')}
      </Alert>
      <FormGroup label={t('Address source')} fieldId="ip-pool">
        {pools === null ? <Spinner size="md" /> : (
          <FormSelect id="ip-pool" value={ea.ipPool} onChange={(_e, val) => choosePool(val)}>
            <FormSelectOption value="" label={t('Enter addresses manually')} />
            {pools.map((p) => {
              const free = freeAddresses(p).length;
              return (
                <FormSelectOption
                  key={p.name}
                  value={p.name}
                  isDisabled={free < 2}
                  label={
                    p.problem
                      ? t('{{name}}: invalid pool', { name: p.name })
                      : t('{{name}}: {{nad}}, {{free}} free', { name: p.name, nad: p.nadName, free: String(free) })
                  }
                />
              );
            })}
          </FormSelect>
        )}
        <FormHelperText>
          <HelperText>
            <HelperTextItem>
              {pools && pools.length === 0
                ? t('No IP pools yet. Cluster administrators can prepare free addresses in the IP Pools tab.')
                : t('Pick an IP pool to choose the network and free addresses from a list. Pools with fewer than two free addresses cannot be picked.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('External network (NAD)')} isRequired fieldId="nad-name">
        {pool ? (
          <TextInput id="nad-name" value={pool.nadName} isDisabled aria-label={t('External network (NAD)')} />
        ) : nadsLoading ? <Spinner size="md" /> : (
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
        {pool ? (
          <FormSelect id="api-ip" value={ea.apiIP} onChange={(_e, val) => onChange({ apiIP: val })} validated={apiErr ? 'error' : 'default'}>
            <FormSelectOption value="" label={t('Select a free address')} isDisabled />
            {poolChoices(ea.apiIP).map((ip) => (
              <FormSelectOption key={ip} value={ip} label={ip} isDisabled={ip === ea.ingressIP} />
            ))}
          </FormSelect>
        ) : (
          <TextInput
            id="api-ip"
            value={ea.apiIP}
            onChange={(_e, val) =>
              // Offer the usual gateway (first host of the subnet) until the user sets one.
              onChange({ apiIP: val, ...(ea.gateway ? {} : { gateway: suggestGateway(val, ea.prefixLength) }) })
            }
            validated={apiErr ? 'error' : 'default'}
            placeholder="192.168.100.10"
          />
        )}
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={apiErr ? 'error' : 'default'}>
              {apiErr || t('Free address on the external VLAN for api.<cluster>.<domain>.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('External Ingress IP')} isRequired fieldId="ingress-ip">
        {pool ? (
          <FormSelect id="ingress-ip" value={ea.ingressIP} onChange={(_e, val) => onChange({ ingressIP: val })} validated={ingressErr ? 'error' : 'default'}>
            <FormSelectOption value="" label={t('Select a free address')} isDisabled />
            {poolChoices(ea.ingressIP).map((ip) => (
              <FormSelectOption key={ip} value={ip} label={ip} isDisabled={ip === ea.apiIP} />
            ))}
          </FormSelect>
        ) : (
          <TextInput
            id="ingress-ip"
            value={ea.ingressIP}
            onChange={(_e, val) => onChange({ ingressIP: val })}
            validated={ingressErr ? 'error' : 'default'}
            placeholder="192.168.100.11"
          />
        )}
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
          isDisabled={Boolean(pool)}
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
      <FormGroup label={t('VLAN gateway')} isRequired fieldId="gateway">
        <TextInput
          id="gateway"
          value={ea.gateway}
          isDisabled={Boolean(pool)}
          onChange={(_e, val) => onChange({ gateway: val })}
          validated={gatewayErr ? 'error' : 'default'}
          placeholder="192.168.100.1"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={gatewayErr ? 'error' : 'default'}>
              {gatewayErr || t('Router of the external VLAN. The cluster answers clients outside the VLAN through it.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('Host routes')} fieldId="host-routes">
        <Checkbox
          id="host-routes"
          isChecked={Boolean(ea.hostRoutes) && Boolean(ea.ipPool)}
          isDisabled={!ea.ipPool}
          onChange={(_e, v) => onChange({ hostRoutes: v })}
          label={t("Publish the API and console through this cluster's routes (no DNS records needed)")}
          description={!ea.ipPool ? t('Pick the addresses from an IP pool to enable host routes.') : t('Console, login and API get names under this cluster\'s *.apps domain, signed by a per-cluster CA you can download. Publish your own applications with the option below or per route. The external VLAN must be reachable from this cluster\'s nodes.')}
        />
        <Checkbox
          id="publish-app-routes"
          isChecked={Boolean(ea.hostRoutes && ea.ipPool && ea.publishAppRoutes)}
          isDisabled={!ea.hostRoutes || !ea.ipPool}
          onChange={(_e, v) => onChange({ publishAppRoutes: v })}
          label={t('Also publish all application routes')}
          description={t('Every route you create in the nested cluster (outside openshift-* and kube-* namespaces) gets an address under this cluster\'s *.apps with its trusted certificate, within about two minutes. Opt a route out with the annotation sos.octools.io/publish-host-route=false. The hop from this cluster\'s router to the nested cluster is plain HTTP.')}
        />
      </FormGroup>

      {fqdn && ea.apiIP && ea.ingressIP && !apiErr && !ingressErr && (
        <Alert variant="info" isInline title={t('DNS Records')} className="sos-dns-records">
          {ea.hostRoutes && (
            <p>{t('Optional with host routes; needed for other applications under *.apps of the nested cluster.')}</p>
          )}
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
