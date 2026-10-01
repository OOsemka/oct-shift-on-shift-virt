import React, { FC, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  InputGroup,
  InputGroupItem,
  Spinner,
  TextInput,
} from '@patternfly/react-core';
import { PlusCircleIcon } from '@patternfly/react-icons';
import { K8sResourceCommon, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { ClusterConfig } from './types';
import {
  validateClusterName,
  validateDomain,
  validateNamespace,
} from '../../utils/validation';
import { fetchOCPVersions } from '../../utils/api';

const I18N = 'plugin__oct-shift-on-shift-virt';

const FALLBACK_VERSIONS = ['4.22', '4.21', '4.20', '4.19', '4.18'];

type NamespaceKind = K8sResourceCommon & {
  metadata: { name: string; labels?: Record<string, string> };
};

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
}

const ClusterBasicsStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);
  const [ocpVersions, setOcpVersions] = useState<string[]>(FALLBACK_VERSIONS);
  const [versionsLoading, setVersionsLoading] = useState(true);
  const [createNewNs, setCreateNewNs] = useState(false);
  const [nsFilter, setNsFilter] = useState('');

  const [allNamespaces, nsLoaded] = useK8sWatchResource<NamespaceKind[]>({
    groupVersionKind: { version: 'v1', kind: 'Namespace' },
    isList: true,
  });

  const nsList = useMemo(() => {
    const items = Array.isArray(allNamespaces) ? allNamespaces : [];
    const filtered = items.filter(
      (ns) => ns.metadata?.labels?.['k8s.ovn.org/primary-user-defined-network'] !== undefined,
    );
    const names = filtered.map((ns) => ns.metadata?.name || '').filter(Boolean).sort();
    if (!nsFilter) return names;
    return names.filter((n) => n.includes(nsFilter.toLowerCase()));
  }, [allNamespaces, nsFilter]);

  useEffect(() => {
    fetchOCPVersions()
      .then((data: unknown) => {
        const resp = data as { items?: string[] };
        if (resp.items?.length) setOcpVersions(resp.items);
      })
      .catch(() => {})
      .finally(() => setVersionsLoading(false));
  }, []);

  const nameError = config.clusterName ? validateClusterName(config.clusterName) : undefined;
  const domainError = config.baseDomain ? validateDomain(config.baseDomain) : undefined;
  const nsError = config.namespace ? validateNamespace(config.namespace) : undefined;

  return (
    <Form className="sos-form">
      <FormGroup label={t('Cluster Name')} isRequired fieldId="cluster-name">
        <TextInput
          id="cluster-name"
          value={config.clusterName}
          onChange={(_e, val) => {
            const patch: Partial<ClusterConfig> = { clusterName: val };
            if (!config.namespace || config.namespace === `${config.clusterName}-ns`) {
              patch.namespace = `${val}-ns`;
            }
            onChange(patch);
          }}
          validated={nameError ? 'error' : 'default'}
          placeholder="my-cluster"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={nameError ? 'error' : 'default'}>
              {nameError || t('Enter a DNS-safe cluster name (lowercase, alphanumeric, hyphens)')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('Base Domain')} isRequired fieldId="base-domain">
        <TextInput
          id="base-domain"
          value={config.baseDomain}
          onChange={(_e, val) => onChange({ baseDomain: val })}
          validated={domainError ? 'error' : 'default'}
          placeholder="example.com"
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={domainError ? 'error' : 'default'}>
              {domainError || t('Enter the base domain (e.g. example.com)')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('Namespace')} isRequired fieldId="namespace">
        {!nsLoaded ? <Spinner size="md" /> : createNewNs ? (
          <>
            <InputGroup>
              <InputGroupItem isFill>
                <TextInput
                  id="namespace"
                  value={config.namespace}
                  onChange={(_e, val) => onChange({ namespace: val })}
                  validated={nsError ? 'error' : 'default'}
                  placeholder="my-cluster-ns"
                />
              </InputGroupItem>
              <InputGroupItem>
                <Button variant="link" onClick={() => setCreateNewNs(false)}>
                  {t('Use existing')}
                </Button>
              </InputGroupItem>
            </InputGroup>
            <FormHelperText>
              <HelperText>
                <HelperTextItem variant={nsError ? 'error' : 'default'}>
                  {nsError || t('Enter a name for the new namespace (will be created during deployment)')}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </>
        ) : (
          <>
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  {t('Only namespaces with primary user-defined network support are shown.')}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
            <TextInput
              id="namespace-filter"
              value={nsFilter}
              onChange={(_e, val) => setNsFilter(val)}
              placeholder={t('Type to filter namespaces...')}
              style={{ marginBottom: '4px' }}
            />
            <InputGroup>
              <InputGroupItem isFill>
                <FormSelect
                  id="namespace"
                  value={config.namespace}
                  onChange={(_e, val) => onChange({ namespace: val })}
                >
                  <FormSelectOption key="" value="" label={t('Select a namespace')} isDisabled />
                  {nsList.map((ns) => (
                    <FormSelectOption key={ns} value={ns} label={ns} />
                  ))}
                </FormSelect>
              </InputGroupItem>
              <InputGroupItem>
                <Button
                  variant="link"
                  icon={<PlusCircleIcon />}
                  onClick={() => {
                    setCreateNewNs(true);
                    onChange({ namespace: `${config.clusterName || 'cluster'}-ns` });
                  }}
                >
                  {t('Create new')}
                </Button>
              </InputGroupItem>
            </InputGroup>
          </>
        )}
      </FormGroup>

      <FormGroup label={t('OCP Version')} isRequired fieldId="ocp-version">
        {versionsLoading ? <Spinner size="md" /> : (
          <FormSelect
            id="ocp-version"
            value={config.ocpVersion}
            onChange={(_e, val) => onChange({ ocpVersion: val })}
          >
            <FormSelectOption key="" value="" label={t('Select the OpenShift version to deploy')} isDisabled />
            {ocpVersions.map((v) => (
              <FormSelectOption key={v} value={v} label={`OpenShift ${v}`} />
            ))}
          </FormSelect>
        )}
      </FormGroup>
    </Form>
  );
};

export default ClusterBasicsStep;
