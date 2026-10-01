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
import { validateCIDR } from '../../utils/validation';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig['network']>) => void;
}

const NetworkStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);
  const net = config.network;

  const cudnErr = net.cudnSubnet ? validateCIDR(net.cudnSubnet) : undefined;
  const clusterErr = net.clusterNetworkCIDR ? validateCIDR(net.clusterNetworkCIDR) : undefined;
  const serviceErr = net.serviceNetworkCIDR ? validateCIDR(net.serviceNetworkCIDR) : undefined;

  return (
    <Form className="sos-form">
      <FormGroup label={t('cUDN Subnet')} isRequired fieldId="cudn-subnet">
        <TextInput
          id="cudn-subnet"
          value={net.cudnSubnet}
          onChange={(_e, val) => onChange({ cudnSubnet: val })}
          validated={cudnErr ? 'error' : 'default'}
          placeholder="10.100.0.0/16"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={cudnErr ? 'error' : 'default'}>
              {cudnErr || t('Internal VM network subnet. API and Ingress VIPs are auto-assigned within this range.')}
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
        {clusterErr && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="error">{clusterErr}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        )}
      </FormGroup>

      <FormGroup label={t('Service Network CIDR')} isRequired fieldId="service-cidr">
        <TextInput
          id="service-cidr"
          value={net.serviceNetworkCIDR}
          onChange={(_e, val) => onChange({ serviceNetworkCIDR: val })}
          validated={serviceErr ? 'error' : 'default'}
          placeholder="172.30.0.0/16"
        />
        {serviceErr && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="error">{serviceErr}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        )}
      </FormGroup>
    </Form>
  );
};

export default NetworkStep;
