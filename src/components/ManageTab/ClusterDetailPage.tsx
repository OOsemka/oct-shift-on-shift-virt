import React, { FC, useCallback, useEffect, useState } from 'react';
import { useParams } from 'react-router-dom-v5-compat';
import { useTranslation } from 'react-i18next';
import { ListPageHeader } from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Breadcrumb,
  BreadcrumbItem,
  Button,
  ClipboardCopy,
  CodeBlock,
  CodeBlockCode,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  ExpandableSection,
  Label,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  PageSection,
  ProgressStepper,
  ProgressStep,
  Spinner,
  Split,
  SplitItem,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import CommunityDisclaimer from '../shared/CommunityDisclaimer';
import { deleteCluster, fetchCluster, fetchKubeadmin, fetchKubeconfig } from '../../utils/api';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface ClusterDetail {
  name: string;
  namespace: string;
  spec: {
    clusterName: string;
    baseDomain: string;
    ocpVersion: string;
    topology: string;
    externalAccess: {
      apiIP: string;
      ingressIP: string;
    };
  };
  status: {
    phase: string;
    message: string;
    apiEndpoint: string;
    consoleURL: string;
    ingressDomain: string;
    nodes: Array<{
      name: string;
      role: string;
      vmStatus: string;
      powerState: string;
      bootPhase: string;
      agentStatus: string;
      ip: string;
    }>;
    phaseHistory: Array<{
      phase: string;
      startedAt: string;
      endedAt: string;
      success: boolean;
      message: string;
    }>;
  };
}

const PHASE_COLORS: Record<string, 'green' | 'blue' | 'orange' | 'red' | 'grey'> = {
  Ready: 'green',
  Failed: 'red',
};

const PHASE_VARIANT: Record<string, 'success' | 'info' | 'warning' | 'danger' | 'pending'> = {
  Ready: 'success',
  Failed: 'danger',
};

const ClusterDetailPage: FC = () => {
  const { name } = useParams<{ name: string }>();
  const { t } = useTranslation(I18N);
  const [cluster, setCluster] = useState<ClusterDetail>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();
  const [showDelete, setShowDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [kubeadminPwd, setKubeadminPwd] = useState<string>();

  const loadCluster = useCallback(() => {
    if (!name) return;
    fetchCluster(name)
      .then((data) => setCluster(data as ClusterDetail))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, [name]);

  useEffect(() => {
    loadCluster();
    const isInstalling = cluster?.status?.phase && !['Ready', 'Failed'].includes(cluster.status.phase);
    const interval = setInterval(loadCluster, isInstalling ? 5000 : 30000);
    return () => clearInterval(interval);
  }, [loadCluster, cluster?.status?.phase]);

  const handleDelete = async () => {
    if (!name) return;
    setDeleting(true);
    try {
      await deleteCluster(name);
      window.location.href = '/community-tools/compute/shift-on-shift-virt';
    } catch {
      setDeleting(false);
    }
  };

  const handleDownloadKubeconfig = async () => {
    if (!name) return;
    try {
      const data = (await fetchKubeconfig(name)) as { kubeconfig: string };
      const blob = new Blob([data.kubeconfig], { type: 'text/yaml' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `kubeconfig-${name}`;
      a.click();
      URL.revokeObjectURL(url);
    } catch {
      // silently handled
    }
  };

  const handleShowKubeadmin = async () => {
    if (!name) return;
    try {
      const data = (await fetchKubeadmin(name)) as { password: string };
      setKubeadminPwd(data.password);
    } catch {
      // silently handled
    }
  };

  if (loading) return <Spinner size="lg" />;
  if (error) return <Alert variant="danger" isInline title={error} />;
  if (!cluster) return <Alert variant="warning" isInline title={t('Cluster not found')} />;

  const { spec, status } = cluster;
  const isReady = status.phase === 'Ready';
  const fqdn = `${spec.clusterName}.${spec.baseDomain}`;

  return (
    <>
      <PageSection type="breadcrumb">
        <Breadcrumb>
          <BreadcrumbItem
            component="a"
            onClick={(e) => {
              e.preventDefault();
              window.location.href = '/community-tools/compute/shift-on-shift-virt';
            }}
          >
            {t('Shift on Shift')}
          </BreadcrumbItem>
          <BreadcrumbItem isActive>{spec.clusterName}</BreadcrumbItem>
        </Breadcrumb>
      </PageSection>
      <ListPageHeader title={spec.clusterName} />
      <PageSection>
        <Stack hasGutter>
          <StackItem>
            <CommunityDisclaimer />
          </StackItem>

          <StackItem>
            <Split hasGutter>
              <SplitItem>
                <Label color={PHASE_COLORS[status.phase] || 'blue'}>
                  {status.phase}
                </Label>
              </SplitItem>
              <SplitItem>{status.message}</SplitItem>
            </Split>
          </StackItem>

          {status.phaseHistory && status.phaseHistory.length > 0 && (
            <StackItem>
              <ProgressStepper isVertical>
                {status.phaseHistory.map((ph) => (
                  <ProgressStep
                    key={ph.phase}
                    variant={ph.success ? 'success' : (PHASE_VARIANT[ph.phase] || 'info')}
                    id={ph.phase}
                    titleId={ph.phase}
                    aria-label={ph.phase}
                    description={ph.endedAt ? new Date(ph.endedAt).toLocaleString() : 'In progress'}
                  >
                    {ph.phase}
                  </ProgressStep>
                ))}
              </ProgressStepper>
            </StackItem>
          )}

          {status.phaseHistory && status.phaseHistory.length > 0 && (
            <StackItem>
              <ExpandableSection
                toggleText={t('Deployment Log')}
                isExpanded={!isReady}
              >
                <CodeBlock>
                  <CodeBlockCode>
                    {status.phaseHistory.map((ph) => {
                      const time = ph.startedAt ? new Date(ph.startedAt).toLocaleTimeString() : '';
                      const icon = ph.success ? '[OK]' : '[..] ';
                      return `${time} ${icon} ${ph.phase}${ph.message ? ' - ' + ph.message : ''}`;
                    }).join('\n') + (status.message ? `\n${new Date().toLocaleTimeString()} [>>]  ${status.message}` : '')}
                  </CodeBlockCode>
                </CodeBlock>
              </ExpandableSection>
            </StackItem>
          )}

          {isReady && (
            <StackItem>
              <DescriptionList isHorizontal>
                <DescriptionListGroup>
                  <DescriptionListTerm>{t('Console URL')}</DescriptionListTerm>
                  <DescriptionListDescription>
                    <a href={status.consoleURL} target="_blank" rel="noopener noreferrer">
                      {status.consoleURL}
                    </a>
                  </DescriptionListDescription>
                </DescriptionListGroup>
                <DescriptionListGroup>
                  <DescriptionListTerm>{t('API Endpoint')}</DescriptionListTerm>
                  <DescriptionListDescription>
                    <ClipboardCopy isReadOnly variant="inline-compact">
                      {status.apiEndpoint}
                    </ClipboardCopy>
                  </DescriptionListDescription>
                </DescriptionListGroup>
                <DescriptionListGroup>
                  <DescriptionListTerm>{t('Ingress Domain')}</DescriptionListTerm>
                  <DescriptionListDescription>{status.ingressDomain}</DescriptionListDescription>
                </DescriptionListGroup>
              </DescriptionList>
              <Split hasGutter className="sos-cluster-actions">
                <SplitItem>
                  <Button variant="secondary" onClick={handleDownloadKubeconfig}>
                    {t('Download Kubeconfig')}
                  </Button>
                </SplitItem>
                <SplitItem>
                  <Button variant="secondary" onClick={handleShowKubeadmin}>
                    {t('Copy Kubeadmin Password')}
                  </Button>
                </SplitItem>
              </Split>
              {kubeadminPwd && (
                <ClipboardCopy isReadOnly className="sos-kubeadmin">
                  {kubeadminPwd}
                </ClipboardCopy>
              )}
            </StackItem>
          )}

          <StackItem>
            <DescriptionList isHorizontal>
              <DescriptionListGroup>
                <DescriptionListTerm>{t('DNS Records')}</DescriptionListTerm>
                <DescriptionListDescription>
                  <code>api.{fqdn}</code> → <code>{spec.externalAccess.apiIP}</code>
                  <br />
                  <code>*.apps.{fqdn}</code> → <code>{spec.externalAccess.ingressIP}</code>
                </DescriptionListDescription>
              </DescriptionListGroup>
            </DescriptionList>
          </StackItem>

          {status.nodes && status.nodes.length > 0 && (
            <StackItem>
              <Table aria-label="Node status" variant="compact">
                <Thead>
                  <Tr>
                    <Th>{t('Name')}</Th>
                    <Th>{t('Role')}</Th>
                    <Th>{t('VM Status')}</Th>
                    <Th>{t('Power State')}</Th>
                    <Th>{t('Boot Phase')}</Th>
                    <Th>{t('Agent Status')}</Th>
                    <Th>{t('IP')}</Th>
                  </Tr>
                </Thead>
                <Tbody>
                  {status.nodes.map((node) => (
                    <Tr key={node.name}>
                      <Td>{node.name}</Td>
                      <Td>{node.role}</Td>
                      <Td>{node.vmStatus}</Td>
                      <Td>{node.powerState}</Td>
                      <Td>{node.bootPhase}</Td>
                      <Td>{node.agentStatus}</Td>
                      <Td>{node.ip || '—'}</Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            </StackItem>
          )}

          <StackItem>
            <Button variant="danger" onClick={() => setShowDelete(true)}>
              {t('Delete Cluster')}
            </Button>
          </StackItem>
        </Stack>
      </PageSection>

      {showDelete && (
        <Modal isOpen onClose={() => setShowDelete(false)} variant="small">
          <ModalHeader title={t('Delete Cluster')} />
          <ModalBody>
            <p>{t('Are you sure you want to delete cluster {{name}}?', { name: spec.clusterName })}</p>
            <p>{t('This will remove all VMs, networking, and storage resources.')}</p>
          </ModalBody>
          <ModalFooter>
            <Button variant="danger" onClick={handleDelete} isLoading={deleting} isDisabled={deleting}>
              {t('Delete')}
            </Button>
            <Button variant="link" onClick={() => setShowDelete(false)}>
              {t('Cancel')}
            </Button>
          </ModalFooter>
        </Modal>
      )}
    </>
  );
};

export default ClusterDetailPage;
