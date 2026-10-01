import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  Spinner,
} from '@patternfly/react-core';
import { K8sResourceCommon, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { ClusterConfig } from './types';

const I18N = 'plugin__oct-shift-on-shift-virt';

type StorageClassKind = K8sResourceCommon & {
  metadata: { name: string; annotations?: Record<string, string> };
};

function isDefault(sc: StorageClassKind): boolean {
  return sc.metadata?.annotations?.['storageclass.kubernetes.io/is-default-class'] === 'true';
}

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
}

const StorageStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);

  const [storageClasses, loaded, error] = useK8sWatchResource<StorageClassKind[]>({
    groupVersionKind: { group: 'storage.k8s.io', version: 'v1', kind: 'StorageClass' },
    isList: true,
  });

  const scList = Array.isArray(storageClasses) ? storageClasses : [];

  if (!loaded) return <Spinner size="lg" />;

  return (
    <Form className="sos-form">
      {error && (
        <Alert variant="warning" isInline title={t('Error loading storage classes')}>
          {String(error)}
        </Alert>
      )}
      <FormGroup label={t('Storage Class')} isRequired fieldId="storage-class">
        <FormSelect
          id="storage-class"
          value={config.storageClassName}
          onChange={(_e, val) => onChange({ storageClassName: val })}
        >
          <FormSelectOption key="" value="" label={t('Select a storage class')} isDisabled />
          {scList.map((sc) => {
            const name = sc.metadata?.name || '';
            const suffix = isDefault(sc) ? ` (${t('default')})` : '';
            return <FormSelectOption key={name} value={name} label={`${name}${suffix}`} />;
          })}
        </FormSelect>
      </FormGroup>
    </Form>
  );
};

export default StorageStep;
