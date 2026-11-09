import React, { FC, useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  EmptyState,
  EmptyStateBody,
  Flex,
  FlexItem,
  FormGroup,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  Spinner,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import {
  ClusterListItem,
  fetchClusterList,
  fetchMasters,
  makeMaster,
  masterCandidates,
  refreshMaster,
  MasterItem,
} from '../../utils/masters';

const I18N = 'plugin__oct-shift-on-shift-virt';

const errText = (e: unknown): string => {
  const err = e as { json?: { error?: string }; message?: string };
  return err?.json?.error || err?.message || String(e);
};

const MastersTab: FC = () => {
  const { t } = useTranslation(I18N);
  const [masters, setMasters] = useState<MasterItem[] | null>(null);
  const [clusters, setClusters] = useState<ClusterListItem[]>([]);
  const [error, setError] = useState<string>();
  const [selected, setSelected] = useState('');
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);

  const load = useCallback(() => {
    Promise.all([fetchMasters(), fetchClusterList()])
      .then(([m, c]) => {
        setMasters(m);
        setClusters(c);
        setError(undefined);
      })
      .catch((e) => setError(errText(e)));
  }, []);
  useEffect(() => {
    load();
    const id = setInterval(load, 15000);
    return () => clearInterval(id);
  }, [load]);

  const candidates = masterCandidates(clusters);
  const doRefresh = async (name: string) => {
    try {
      await refreshMaster(name);
      load();
    } catch (e) {
      setError(errText(e));
    }
  };
  const inProgress = clusters.filter((c) => c.phase === 'MakeMaster');
  const doMake = async () => {
    setBusy(true);
    try {
      await makeMaster(selected);
      setConfirming(false);
      setSelected('');
      load();
    } catch (e) {
      setError(errText(e));
      setConfirming(false);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Stack hasGutter>
      <StackItem>
        <Alert variant="info" isInline title={t('Shift-on-Shift Express')}>
          <p>
            {t(
              'A master is an installed cluster kept as a template: it is shut down and its disks are snapshotted. Clones are created from it in a few minutes instead of a full install, each with its own name, external addresses, host routes and credentials.',
            )}
          </p>
          <p>
            {t(
              'A clone is usable within minutes. It is fully isolated from its master and other clones (own admin certificates, service-account issuer and API CAs) when its Isolated condition turns True, about 15 minutes later.',
            )}
          </p>
        </Alert>
      </StackItem>
      {error && (
        <StackItem>
          <Alert variant="danger" isInline title={error} />
        </StackItem>
      )}
      <StackItem>
        <Card isCompact>
          <CardTitle>{t('Masters')}</CardTitle>
          <CardBody>
            {masters === null ? (
              <Spinner size="md" />
            ) : masters.length === 0 ? (
              <EmptyState>
                <EmptyStateBody>{t('No masters yet. Make one from a Ready cluster below.')}</EmptyStateBody>
              </EmptyState>
            ) : (
              <Table variant="compact" aria-label={t('Masters')}>
                <Thead>
                  <Tr>
                    <Th>{t('Name')}</Th>
                    <Th>{t('OpenShift')}</Th>
                    <Th>{t('Nodes')}</Th>
                    <Th>{t('Internal name')}</Th>
                    <Th>{t('Created')}</Th>
                    <Th>{t('Clones')}</Th>
                    <Th>{t('Snapshots taken')}</Th>
                    <Th />
                  </Tr>
                </Thead>
                <Tbody>
                  {masters.map((m) => (
                    <Tr key={m.name}>
                      <Td>{m.name}</Td>
                      <Td>{m.releaseVersion || m.ocpVersion}</Td>
                      <Td>{`${m.masters.count} × ${m.masters.cpuCores} vCPU / ${m.masters.memoryGi} GiB / ${m.masters.diskGi} GiB`}</Td>
                      <Td>{m.internalDomain}</Td>
                      <Td>{m.createdAt ? new Date(m.createdAt).toLocaleString() : ''}</Td>
                      <Td>{m.clones}</Td>
                      <Td>{new Date(m.refreshedAt || m.createdAt || '').toLocaleString()}</Td>
                      <Td>
                        {m.refreshing ? (
                          <Spinner size="sm" aria-label={t('Refreshing')} />
                        ) : (
                          <Button variant="link" isInline isDisabled={m.clones > 0} onClick={() => doRefresh(m.name)}>
                            {t('Refresh')}
                          </Button>
                        )}
                      </Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            )}
            <HelperText>
              <HelperTextItem>
                {t('Clone from a master with the quick cluster card on the Install tab. A master cannot be deleted while it has clones.')}{' '}
                {t('Refresh a master every few days: it boots, renews its certificates and takes new snapshots (about 20 minutes), so its clones start faster. Only possible while it has no clones.')}
              </HelperTextItem>
            </HelperText>
          </CardBody>
        </Card>
      </StackItem>
      <StackItem>
        <Card isCompact>
          <CardTitle>{t('Make a master')}</CardTitle>
          <CardBody>
            {inProgress.map((c) => (
              <Alert key={c.name} variant="info" isInline isPlain title={t('{{name}}: shutting down and snapshotting…', { name: c.name })} />
            ))}
            <Flex alignItems={{ default: 'alignItemsFlexEnd' }} spaceItems={{ default: 'spaceItemsMd' }}>
              <FlexItem>
                <FormGroup label={t('Ready cluster')} fieldId="master-candidate">
                  <FormSelect id="master-candidate" value={selected} onChange={(_e, v) => setSelected(v)}>
                    <FormSelectOption value="" label={candidates.length ? t('Select a cluster') : t('No eligible cluster')} />
                    {candidates.map((c) => (
                      <FormSelectOption key={c.name} value={c.name} label={`${c.name} (OpenShift ${c.ocpVersion})`} />
                    ))}
                  </FormSelect>
                </FormGroup>
              </FlexItem>
              <FlexItem>
                <Button variant="secondary" isDisabled={!selected} onClick={() => setConfirming(true)}>
                  {t('Make master')}
                </Button>
              </FlexItem>
            </Flex>
            <HelperText>
              <HelperTextItem>
                {t('Eligible: Ready clusters installed with this tool, with the bastion load balancer and without nested storage or virtualization (support for those comes later). Clones are not eligible.')}
              </HelperTextItem>
            </HelperText>
          </CardBody>
        </Card>
      </StackItem>
      <Modal isOpen={confirming} onClose={() => setConfirming(false)} variant="small">
        <ModalHeader title={t('Make {{name}} a master?', { name: selected })} />
        <ModalBody>
          <p>
            {t(
              'The cluster is shut down for good and its disks are snapshotted. It stops being a usable cluster: it becomes the template for clones. Clones keep its internal name and current state, so prepare it (operators, content) before making it a master.',
            )}
          </p>
        </ModalBody>
        <ModalFooter>
          <Button variant="danger" onClick={doMake} isLoading={busy} isDisabled={busy}>
            {t('Make master')}
          </Button>
          <Button variant="link" onClick={() => setConfirming(false)}>
            {t('Cancel')}
          </Button>
        </ModalFooter>
      </Modal>
    </Stack>
  );
};

export default MastersTab;
