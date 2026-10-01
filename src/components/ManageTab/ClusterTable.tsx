import React, { FC, useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  EmptyState,
  EmptyStateBody,
  EmptyStateActions,
  EmptyStateFooter,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  Spinner,
} from '@patternfly/react-core';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import { deleteCluster, fetchClusters } from '../../utils/api';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface ClusterListItem {
  name: string;
  namespace: string;
  phase: string;
  consoleURL: string;
  apiEndpoint: string;
  createdAt: string;
}

const PHASE_COLORS: Record<string, 'green' | 'blue' | 'orange' | 'red' | 'grey'> = {
  Ready: 'green',
  Installing: 'blue',
  Provisioning: 'orange',
  Error: 'red',
};

const ClusterTable: FC = () => {
  const { t } = useTranslation(I18N);
  const [clusters, setClusters] = useState<ClusterListItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [deleteTarget, setDeleteTarget] = useState<string>();
  const [deleting, setDeleting] = useState(false);

  const loadClusters = useCallback(() => {
    setLoading(true);
    fetchClusters()
      .then((data: unknown) => {
        const items = data as { items?: ClusterListItem[] };
        setClusters(items.items || []);
      })
      .catch(() => setClusters([]))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    loadClusters();
    const interval = setInterval(loadClusters, 15000);
    return () => clearInterval(interval);
  }, [loadClusters]);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteCluster(deleteTarget);
      setDeleteTarget(undefined);
      loadClusters();
    } catch {
      // error handling via reload
    } finally {
      setDeleting(false);
    }
  };

  if (loading && clusters.length === 0) return <Spinner size="lg" />;

  if (clusters.length === 0) {
    return (
      <EmptyState>
        <EmptyStateBody>{t('No clusters deployed yet.')}</EmptyStateBody>
        <EmptyStateFooter>
          <EmptyStateActions>
            <p>{t('Go to the Install tab to create one.')}</p>
          </EmptyStateActions>
        </EmptyStateFooter>
      </EmptyState>
    );
  }

  return (
    <>
      <Table aria-label="Deployed clusters" variant="compact">
        <Thead>
          <Tr>
            <Th>{t('Name')}</Th>
            <Th>{t('Namespace')}</Th>
            <Th>{t('Status')}</Th>
            <Th>{t('Console URL')}</Th>
            <Th>{t('API Endpoint')}</Th>
            <Th />
          </Tr>
        </Thead>
        <Tbody>
          {clusters.map((c) => (
            <Tr key={c.name}>
              <Td>
                <a href={`/community-tools/compute/shift-on-shift-virt/cluster/${encodeURIComponent(c.name)}`}>
                  {c.name}
                </a>
              </Td>
              <Td>{c.namespace}</Td>
              <Td>
                <Label color={PHASE_COLORS[c.phase] || 'grey'}>{c.phase}</Label>
              </Td>
              <Td>
                {c.consoleURL ? (
                  <a href={c.consoleURL} target="_blank" rel="noopener noreferrer">
                    {c.consoleURL}
                  </a>
                ) : (
                  '—'
                )}
              </Td>
              <Td>{c.apiEndpoint || '—'}</Td>
              <Td>
                <Button
                  variant="danger"
                  size="sm"
                  onClick={() => setDeleteTarget(c.name)}
                >
                  {t('Delete')}
                </Button>
              </Td>
            </Tr>
          ))}
        </Tbody>
      </Table>

      {deleteTarget && (
        <Modal
          isOpen
          onClose={() => setDeleteTarget(undefined)}
          variant="small"
        >
          <ModalHeader title={t('Delete Cluster')} />
          <ModalBody>
            <p>{t('Are you sure you want to delete cluster {{name}}?', { name: deleteTarget })}</p>
            <p>{t('This will remove all VMs, networking, and storage resources.')}</p>
          </ModalBody>
          <ModalFooter>
            <Button
              variant="danger"
              onClick={handleDelete}
              isLoading={deleting}
              isDisabled={deleting}
            >
              {t('Delete')}
            </Button>
            <Button variant="link" onClick={() => setDeleteTarget(undefined)}>
              {t('Cancel')}
            </Button>
          </ModalFooter>
        </Modal>
      )}
    </>
  );
};

export default ClusterTable;
