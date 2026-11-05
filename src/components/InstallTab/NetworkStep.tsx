import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  TextInput,
} from '@patternfly/react-core';
import { ClusterConfig } from './types';
import { internalVIPs, NetworkField, networkProblems } from '../../utils/network-check';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig['network']>) => void;
}

const NetworkStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);
  const net = config.network;
  const problems = networkProblems(net);
  const errorFor = (field: NetworkField) => {
    const p = problems.find((x) => x.field === field);
    return p ? t(p.key, p.values) : undefined;
  };
  const cudnErr = errorFor('cudnSubnet');
  const clusterErr = errorFor('clusterNetworkCIDR');
  const serviceErr = errorFor('serviceNetworkCIDR');
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
    </Form>
  );
};

export default NetworkStep;
