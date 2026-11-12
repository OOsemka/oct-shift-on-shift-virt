import React, { FC, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Button,
  Checkbox,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  NumberInput,
  Radio,
} from '@patternfly/react-core';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { enableStorage } from '../../utils/api';
import {
  isDefault,
  isDefaultVirt,
  preferredInfraClass,
  StorageClassKind,
  StorageProfileKind,
  supportsRwxBlock,
  VIRT_BOOT_SOURCES_GI,
} from '../InstallTab/StorageStep';

const I18N = 'plugin__oct-shift-on-shift-virt';
const DEFAULT_ALLOWANCE_GI = 100; // same default as the wizard

interface Props {
  clusterName: string;
  /** Add OpenShift Virtualization (which includes nested storage) or nested storage only. */
  virtualization: boolean;
  /** The cluster already has working nested storage (its settings are kept). */
  storageReady: boolean;
  /** Masters + workers + infra: 1 means the cluster is unavailable while its VM restarts. */
  nodeCount: number;
  onClose: () => void;
  onStarted: () => void;
}

/**
 * Day-2 action on a Ready cluster: add nested storage, or OpenShift
 * Virtualization (which needs nested storage and a restart of the node VMs,
 * so the host CPU's hardware virtualization reaches the nodes: rolling by
 * default, or all at once if the user prefers speed over availability).
 */
const EnableStorageModal: FC<Props> = ({ clusterName, virtualization, storageReady, nodeCount, onClose, onStarted }) => {
  const { t } = useTranslation(I18N);
  const [storageClasses, scLoaded] = useK8sWatchResource<StorageClassKind[]>({
    groupVersionKind: { group: 'storage.k8s.io', version: 'v1', kind: 'StorageClass' },
    isList: true,
  });
  const [storageProfiles] = useK8sWatchResource<StorageProfileKind[]>({
    groupVersionKind: { group: 'cdi.kubevirt.io', version: 'v1beta1', kind: 'StorageProfile' },
    isList: true,
  });
  const scList = Array.isArray(storageClasses) ? storageClasses : [];
  const [infra, setInfra] = useState('');
  const [allowance, setAllowance] = useState(DEFAULT_ALLOWANCE_GI);
  const [restartMode, setRestartMode] = useState<'rolling' | 'simultaneous'>('rolling');
  const [restartAck, setRestartAck] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string>();

  useEffect(() => {
    if (!infra && scLoaded && scList.length) setInfra(preferredInfraClass(scList));
  }, [infra, scLoaded, scList]);

  const configureStorage = !storageReady;
  const rwxBlock = infra ? supportsRwxBlock(Array.isArray(storageProfiles) ? storageProfiles : [], infra) : undefined;
  const single = nodeCount <= 1;
  const allAtOnce = single || restartMode === 'simultaneous';
  const chooseRestart = (m: 'rolling' | 'simultaneous') => {
    setRestartMode(m);
    setRestartAck(false); // the acknowledgement names the chosen mode
  };
  const canSubmit = !submitting && (!virtualization || restartAck) && (!configureStorage || allowance >= 0);

  const submit = async () => {
    setSubmitting(true);
    setError(undefined);
    try {
      await enableStorage(clusterName, {
        virtualization,
        ...(virtualization && !single ? { restartMode } : {}),
        ...(configureStorage ? { infraStorageClassName: infra, allowanceGi: allowance } : {}),
      });
      onStarted();
    } catch (e) {
      setError((e as Error)?.message || String(e));
      setSubmitting(false);
    }
  };

  return (
    <Modal isOpen onClose={onClose} variant="medium">
      <ModalHeader title={virtualization ? t('Add OpenShift Virtualization') : t('Add nested storage')} />
      <ModalBody>
        <Form>
          {virtualization && configureStorage && (
            <Alert variant="info" isInline title={t('This also adds nested storage')}>
              {t(
                'OpenShift Virtualization keeps its virtual machine disks on nested storage, so nested storage is configured first (a StorageClass inside {{cluster}} backed by host storage).',
                { cluster: clusterName },
              )}
            </Alert>
          )}
          {virtualization && !configureStorage && (
            <Alert variant="info" isInline title={t('Uses the existing nested storage')}>
              {t('The cluster already has nested storage; its settings are kept.')}
            </Alert>
          )}
          {virtualization && !single && (
            <FormGroup role="radiogroup" isInline label={t('Node restart')} fieldId="enable-restart-mode">
              <Radio
                id="enable-restart-rolling"
                name="enable-restart-mode"
                isChecked={restartMode === 'rolling'}
                onChange={() => chooseRestart('rolling')}
                label={t('Rolling (one node at a time)')}
                description={t('The cluster stays available.')}
              />
              <Radio
                id="enable-restart-simultaneous"
                name="enable-restart-mode"
                isChecked={restartMode === 'simultaneous'}
                onChange={() => chooseRestart('simultaneous')}
                label={t('All nodes at once')}
                description={t('Faster; the cluster is unavailable while it restarts.')}
              />
            </FormGroup>
          )}
          {virtualization &&
            (single ? (
              <Alert variant="danger" isInline title={t('The cluster restarts and is unavailable for a while')}>
                {t(
                  'To pass the host CPU’s hardware virtualization through, the node’s VM must be restarted. {{cluster}} has a single node, so the cluster and its workloads are unavailable until the node has booted again.',
                  { cluster: clusterName },
                )}
              </Alert>
            ) : allAtOnce ? (
              <Alert variant="danger" isInline title={t('All nodes restart at once')}>
                {t(
                  'To pass the host CPU’s hardware virtualization through, each node’s VM must be restarted. All {{count}} nodes of {{cluster}} are stopped and started together, without draining: the cluster, its API and its workloads are unavailable until every node has booted again and the control plane has settled.',
                  { count: nodeCount, cluster: clusterName },
                )}
              </Alert>
            ) : (
              <Alert variant="warning" isInline title={t('Rolling restart of every node')}>
                {t(
                  'To pass the host CPU’s hardware virtualization through, each node’s VM must be restarted. The {{count}} nodes of {{cluster}} are drained and restarted one at a time (workers first, masters last); each must boot again and the control plane must settle before the next one. Workloads move between nodes while this runs.',
                  { count: nodeCount, cluster: clusterName },
                )}
              </Alert>
            ))}

          {configureStorage && (
            <>
              <FormGroup label={t('Host StorageClass')} fieldId="enable-infra-sc">
                <FormSelect id="enable-infra-sc" value={infra} onChange={(_e, v) => setInfra(v)} aria-label={t('Host StorageClass')}>
                  {scList.map((sc) => {
                    const tags = [isDefault(sc) && t('default'), isDefaultVirt(sc) && t('default for virtualization')].filter(Boolean);
                    return (
                      <FormSelectOption
                        key={sc.metadata.name}
                        value={sc.metadata.name}
                        label={tags.length ? `${sc.metadata.name} (${tags.join(', ')})` : sc.metadata.name}
                      />
                    );
                  })}
                </FormSelect>
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>
                      {t('Nested volumes are created as volumes of this class on the host.')}
                    </HelperTextItem>
                    {rwxBlock === false && (
                      <HelperTextItem variant="warning">
                        {t('This class cannot bind ReadWriteMany Block volumes, so nested VMs will not live-migrate.')}
                      </HelperTextItem>
                    )}
                  </HelperText>
                </FormHelperText>
              </FormGroup>
              <FormGroup label={t('Storage allowance (GiB)')} fieldId="enable-allowance">
                <NumberInput
                  id="enable-allowance"
                  value={allowance}
                  min={0}
                  onMinus={() => setAllowance((a) => Math.max(0, a - 10))}
                  onPlus={() => setAllowance((a) => a + 10)}
                  onChange={(e) => setAllowance(Math.max(0, Number((e.target as HTMLInputElement).value) || 0))}
                  inputAriaLabel={t('Storage allowance (GiB)')}
                  unit="GiB"
                />
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>
                      {virtualization
                        ? t('Host storage the nested cluster may use for its volumes, in addition to the VM disks and about {{gi}} GiB of virtualization boot images.', { gi: VIRT_BOOT_SOURCES_GI })
                        : t('Host storage the nested cluster may use for its volumes, in addition to the VM disks.')}
                    </HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FormGroup>
            </>
          )}

          {virtualization && (
            <Checkbox
              id="enable-restart-ack"
              isChecked={restartAck}
              onChange={(_e, v) => setRestartAck(v)}
              label={
                allAtOnce
                  ? t('I understand that {{cluster}} will restart and be unavailable for a while', { cluster: clusterName })
                  : t('I understand that every node of {{cluster}} will be drained and restarted, one at a time', { cluster: clusterName })
              }
            />
          )}
          {error && <Alert variant="danger" isInline title={error} />}
        </Form>
      </ModalBody>
      <ModalFooter>
        <Button variant="primary" onClick={submit} isDisabled={!canSubmit} isLoading={submitting}>
          {virtualization ? t('Add OpenShift Virtualization') : t('Add nested storage')}
        </Button>
        <Button variant="link" onClick={onClose}>
          {t('Cancel')}
        </Button>
      </ModalFooter>
    </Modal>
  );
};

export default EnableStorageModal;
