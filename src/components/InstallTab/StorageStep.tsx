import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Checkbox,
  Form,
  FormGroup,
  FormHelperText,
  FormSection,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  NumberInput,
  Spinner,
} from '@patternfly/react-core';
import { K8sResourceCommon, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { ClusterConfig, NestedStorageConfig } from './types';

const I18N = 'plugin__oct-shift-on-shift-virt';

// Mirrors the orchestrator (storage.go): OpenShift Virtualization imports its
// boot-source images into the nested cluster, all on host storage.
const VIRT_BOOT_SOURCES_GI = 180;
// Per-cluster fixed disks: bastion root (32) plus the install ISO copies.
const BASTION_DISK_GI = 32;
const ISO_PER_NODE_GI = 8;
const ISO_BLOCK_GI = 5;

type StorageProfileKind = K8sResourceCommon & {
  status?: { claimPropertySets?: Array<{ accessModes?: string[]; volumeMode?: string }> };
};

/**
 * Mirrors the orchestrator (finding F25): nested RWX, and so live migration
 * of nested VMs, needs the host class to support RWX Block. undefined = the
 * host profile is not visible, so nothing can be said.
 */
function supportsRwxBlock(profiles: StorageProfileKind[], sc: string): boolean | undefined {
  const p = profiles.find((x) => x.metadata?.name === sc);
  if (!p?.status?.claimPropertySets) return undefined;
  return p.status.claimPropertySets.some(
    (c) => c.volumeMode === 'Block' && (c.accessModes || []).includes('ReadWriteMany'),
  );
}

type StorageClassKind = K8sResourceCommon & {
  metadata: { name: string; annotations?: Record<string, string> };
};

function isDefault(sc: StorageClassKind): boolean {
  return sc.metadata?.annotations?.['storageclass.kubernetes.io/is-default-class'] === 'true';
}

function isDefaultVirt(sc: StorageClassKind): boolean {
  return sc.metadata?.annotations?.['storageclass.kubevirt.io/is-default-virt-class'] === 'true';
}

/** Same rule as the orchestrator: default virt class, else cluster default. */
function preferredInfraClass(list: StorageClassKind[]): string {
  return (list.find(isDefaultVirt) || list.find(isDefault))?.metadata?.name || '';
}

/** Host storage the VMs themselves need, in GiB. */
export function vmDiskGi(config: ClusterConfig): number {
  const nodes = config.nodeNames.length || config.masters.count;
  const disks =
    config.masters.count * config.masters.diskGi +
    (config.topology === 'compact' ? 0 : config.workers.count * config.workers.diskGi) +
    (config.topology === 'custom' ? config.infra.count * config.infra.diskGi : 0);
  return disks + BASTION_DISK_GI + nodes * ISO_PER_NODE_GI + ISO_BLOCK_GI;
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

  const [storageProfiles] = useK8sWatchResource<StorageProfileKind[]>({
    groupVersionKind: { group: 'cdi.kubevirt.io', version: 'v1beta1', kind: 'StorageProfile' },
    isList: true,
  });

  const scList = Array.isArray(storageClasses) ? storageClasses : [];

  if (!loaded) return <Spinner size="lg" />;

  const storageOn = config.storage.enabled || config.virtualization.enabled;
  const setStorage = (patch: Partial<NestedStorageConfig>) =>
    onChange({ storage: { ...config.storage, ...patch } });
  // Pre-select the host class when nested storage is first turned on, so the
  // wizard shows exactly what the orchestrator would otherwise pick.
  const enableStorage = (patch: Partial<ClusterConfig>) => {
    const infra = config.storage.infraStorageClassName || preferredInfraClass(scList);
    onChange({ ...patch, storage: { ...config.storage, ...patch.storage, infraStorageClassName: infra } });
  };

  const infraClass = config.storage.infraStorageClassName || preferredInfraClass(scList);
  const rwxBlock = supportsRwxBlock(Array.isArray(storageProfiles) ? storageProfiles : [], infraClass);

  const allowance = config.storage.allowanceGi > 0 ? config.storage.allowanceGi : 100;
  const vmGi = vmDiskGi(config);
  const virtGi = config.virtualization.enabled ? VIRT_BOOT_SOURCES_GI : 0;
  const totalGi = vmGi + (storageOn ? allowance + virtGi : 0);

  const scOptions = scList.map((sc) => {
    const name = sc.metadata?.name || '';
    const tags = [isDefault(sc) && t('default'), isDefaultVirt(sc) && t('default for virtualization')].filter(
      Boolean,
    );
    const suffix = tags.length ? ` (${tags.join(', ')})` : '';
    return <FormSelectOption key={name} value={name} label={`${name}${suffix}`} />;
  });

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
          {scOptions}
        </FormSelect>
        <FormHelperText>
          <HelperText>
            <HelperTextItem>{t('Holds the VM disks of the nested cluster.')}</HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormSection title={t('Nested cluster storage')}>
        <Checkbox
          id="nested-storage"
          label={t('Persistent storage for the nested cluster')}
          description={t(
            'Installs the KubeVirt CSI driver: the nested cluster gets a default StorageClass (kubevirt-csi) whose volumes are carved from a host StorageClass and attached to the nested nodes.',
          )}
          isChecked={storageOn}
          isDisabled={config.virtualization.enabled}
          onChange={(_e, checked) =>
            checked ? enableStorage({ storage: { ...config.storage, enabled: true } }) : setStorage({ enabled: false })
          }
        />
        <Checkbox
          id="nested-virtualization"
          label={t('OpenShift Virtualization in the nested cluster')}
          description={t(
            'Also installs OpenShift Virtualization and configures it to use nested storage, so the nested cluster can run virtual machines. Includes nested storage.',
          )}
          isChecked={config.virtualization.enabled}
          onChange={(_e, checked) =>
            checked
              ? enableStorage({ virtualization: { enabled: true }, storage: { ...config.storage, enabled: true } })
              : onChange({ virtualization: { enabled: false } })
          }
        />

        {storageOn && (
          <>
            <FormGroup label={t('Host storage class for nested volumes')} fieldId="infra-storage-class">
              <FormSelect
                id="infra-storage-class"
                value={config.storage.infraStorageClassName}
                onChange={(_e, val) => setStorage({ infraStorageClassName: val })}
              >
                <FormSelectOption key="" value="" label={t('Host default')} />
                {scOptions}
              </FormSelect>
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>
                    {t(
                      'The only host class the nested cluster may use. Choose one that supports block volumes; with ReadWriteMany it also allows live migration of nested VMs.',
                    )}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FormGroup>
            <FormGroup label={t('Storage allowance for nested volumes (GiB)')} fieldId="storage-allowance">
              <NumberInput
                id="storage-allowance"
                value={config.storage.allowanceGi}
                min={1}
                onMinus={() => setStorage({ allowanceGi: Math.max(1, allowance - 10) })}
                onPlus={() => setStorage({ allowanceGi: allowance + 10 })}
                onChange={(e) => {
                  const v = parseInt((e.target as HTMLInputElement).value, 10);
                  setStorage({ allowanceGi: Number.isNaN(v) ? 0 : v });
                }}
                unit="GiB"
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>
                    {t('Total size of the PersistentVolumeClaims nested users can create.')}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FormGroup>
          </>
        )}

        {storageOn && rwxBlock === false && (
          <Alert
            variant={config.virtualization.enabled ? 'warning' : 'info'}
            isInline
            title={t('{{sc}} has no ReadWriteMany Block support', { sc: infraClass })}
          >
            {config.virtualization.enabled
              ? t(
                  'Nested volumes will be ReadWriteOnce only, so virtual machines inside the nested cluster cannot be live-migrated. Choose a class that supports ReadWriteMany Block if you need that.',
                )
              : t('Nested volumes will be ReadWriteOnce only (Block or Filesystem).')}
          </Alert>
        )}

        {config.virtualization.enabled && (
          <Alert variant="warning" isInline title={t('OpenShift Virtualization needs a lot of storage')}>
            {t(
              'It downloads about {{gi}} GiB of operating system boot images into the nested cluster as soon as it is installed, on top of the allowance above. Size the nested nodes for the virtual machines you plan to run as well.',
              { gi: VIRT_BOOT_SOURCES_GI },
            )}
          </Alert>
        )}
      </FormSection>

      <Alert
        variant={storageOn ? 'info' : 'custom'}
        isInline
        title={t('Host storage needed: about {{gi}} GiB', { gi: totalGi })}
      >
        <div>{t('VM disks, bastion and install media: {{gi}} GiB', { gi: vmGi })}</div>
        {storageOn && <div>{t('Nested volumes: {{gi}} GiB', { gi: allowance })}</div>}
        {virtGi > 0 && <div>{t('Virtualization boot images: {{gi}} GiB', { gi: virtGi })}</div>}
        {storageOn && (
          <div>
            {t(
              'A storage quota of this size is set on the cluster namespace, so the nested cluster cannot use more than this.',
            )}
          </div>
        )}
      </Alert>
    </Form>
  );
};

export default StorageStep;
