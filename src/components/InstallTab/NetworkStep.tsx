import React, { FC, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  TextInput,
} from '@patternfly/react-core';
import { ClusterConfig, NetworkPlugin, NetworkType } from './types';
import {
  internalVIPs,
  MIN_THIRD_PARTY_CNI_MINOR,
  NetworkField,
  networkPluginProblems,
  networkProblems,
  ocpMinor,
} from '../../utils/network-check';
import { fetchNetworkPlugins } from '../../utils/api';

/** An orchestrator without GET /network-plugins only installs OVN-Kubernetes. */
const FALLBACK_PLUGINS: NetworkPlugin[] = [{ networkType: 'OVNKubernetes', displayName: 'OVN-Kubernetes' }];

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig['network']>) => void;
}

const NetworkStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);
  const net = config.network;
  const problems = [...networkProblems(net), ...networkPluginProblems(config)];
  const [plugins, setPlugins] = useState<NetworkPlugin[]>(FALLBACK_PLUGINS);

  useEffect(() => {
    fetchNetworkPlugins()
      .then((data: unknown) => {
        const items = (data as { items?: NetworkPlugin[] }).items;
        if (items?.length) setPlugins(items);
      })
      .catch(() => {});
  }, []);

  const networkType: NetworkType = net.networkType || 'OVNKubernetes';
  const plugin = plugins.find((p) => p.networkType === networkType);
  const versions = plugin?.versions || [];
  const minor = ocpMinor(config.ocpVersion);
  const thirdPartyAllowed = minor === undefined || minor >= MIN_THIRD_PARTY_CNI_MINOR;
  const selectPlugin = (val: string) => {
    const p = plugins.find((x) => x.networkType === val);
    // Record the version explicitly (newest by default) so the cluster spec says what was installed.
    onChange({ networkType: val as NetworkType, cniVersion: p?.versions?.[0] || '' });
  };
  const errorFor = (field: NetworkField) => {
    const p = problems.find((x) => x.field === field);
    return p ? t(p.key, p.values) : undefined;
  };
  const cudnErr = errorFor('cudnSubnet');
  const clusterErr = errorFor('clusterNetworkCIDR');
  const serviceErr = errorFor('serviceNetworkCIDR');
  const pluginErr = errorFor('networkType');
  const vips = internalVIPs(net.cudnSubnet);

  return (
    <Form className="sos-form">
      <FormGroup label={t('Internal network (cUDN subnet)')} isRequired fieldId="cudn-subnet">
        <TextInput
          id="cudn-subnet"
          value={net.cudnSubnet}
          onChange={(_e, val) => onChange({ cudnSubnet: val })}
          validated={cudnErr ? 'error' : 'default'}
          placeholder="10.100.0.0/16"
        />
        <FormHelperText>
          <HelperText>
            {cudnErr ? (
              <HelperTextItem variant="error">{cudnErr}</HelperTextItem>
            ) : (
              <HelperTextItem>
                {t('Private network for the cluster VMs, only reachable inside this cluster. Its internal API and Ingress VIPs are set automatically: {{api}} and {{ingress}}.', {
                  api: vips?.apiVIP || '—',
                  ingress: vips?.ingressVIP || '—',
                })}
              </HelperTextItem>
            )}
            <HelperTextItem>
              {t('The external addresses people use to reach the cluster are entered on the next step, External Access.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('Cluster Network CIDR')} isRequired fieldId="cluster-cidr">
        <TextInput
          id="cluster-cidr"
          value={net.clusterNetworkCIDR}
          onChange={(_e, val) => onChange({ clusterNetworkCIDR: val })}
          validated={clusterErr ? 'error' : 'default'}
          placeholder="10.128.0.0/14"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={clusterErr ? 'error' : 'default'}>
              {clusterErr || t('Pod network inside the nested cluster.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('Service Network CIDR')} isRequired fieldId="service-cidr">
        <TextInput
          id="service-cidr"
          value={net.serviceNetworkCIDR}
          onChange={(_e, val) => onChange({ serviceNetworkCIDR: val })}
          validated={serviceErr ? 'error' : 'default'}
          placeholder="172.30.0.0/16"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={serviceErr ? 'error' : 'default'}>
              {serviceErr || t('Service network inside the nested cluster.')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('Network plugin')} isRequired fieldId="network-type">
        <FormSelect
          id="network-type"
          value={networkType}
          onChange={(_e, val) => selectPlugin(val)}
          validated={pluginErr ? 'error' : 'default'}
        >
          {plugins.map((p) => (
            <FormSelectOption
              key={p.networkType}
              value={p.networkType}
              label={p.networkType === 'OVNKubernetes' ? t('{{name}} (default)', { name: p.displayName }) : p.displayName}
              isDisabled={p.networkType !== 'OVNKubernetes' && !thirdPartyAllowed && p.networkType !== networkType}
            />
          ))}
        </FormSelect>
        <FormHelperText>
          <HelperText>
            {pluginErr ? (
              <HelperTextItem variant="error">{pluginErr}</HelperTextItem>
            ) : networkType === 'OVNKubernetes' ? (
              <HelperTextItem>
                {plugins.length > 1 && !thirdPartyAllowed
                  ? t('Calico and Cilium need OpenShift 4.{{min}} or later.', { min: String(MIN_THIRD_PARTY_CNI_MINOR) })
                  : t('The OpenShift default network plugin.')}
              </HelperTextItem>
            ) : (
              <HelperTextItem>
                {t('Open-source {{name}} replaces OVN-Kubernetes in the nested cluster. Its images come from public upstream registries, so the cluster needs internet access, and the combination is not supported by Red Hat.', { name: plugin?.displayName || networkType })}
              </HelperTextItem>
            )}
          </HelperText>
        </FormHelperText>
      </FormGroup>

      {networkType !== 'OVNKubernetes' && versions.length > 0 && (
        <FormGroup label={t('{{name}} version', { name: plugin?.displayName || networkType })} isRequired fieldId="cni-version">
          <FormSelect id="cni-version" value={net.cniVersion || versions[0]} onChange={(_e, val) => onChange({ cniVersion: val })}>
            {versions.map((v, i) => (
              <FormSelectOption
                key={v}
                value={v}
                label={i === 0 ? t('{{version}} (latest)', { version: v }) : t('{{version}} (previous)', { version: v })}
              />
            ))}
          </FormSelect>
        </FormGroup>
      )}
    </Form>
  );
};

export default NetworkStep;
