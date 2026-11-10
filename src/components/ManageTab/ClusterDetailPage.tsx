import React, { FC, useCallback, useEffect, useState } from 'react';
import { useParams } from 'react-router-dom-v5-compat';
import { useTranslation } from 'react-i18next';
import { ListPageHeader } from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  HelperText,
  HelperTextItem,
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
import ConfigViewer from './ConfigViewer';
import { deleteCluster, fetchCluster, fetchExternalCA, fetchKubeadmin, fetchKubeconfig } from '../../utils/api';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface ClusterDetail {
  name: string;
  namespace: string;
  spec: {
    clusterName: string;
    // Express: set on clones (the master's internal name is kept).
    source?: { master: string; internalName?: string; internalBaseDomain?: string };
    namespace?: string;
    baseDomain: string;
    ocpVersion: string;
    topology: string;
    network?: {
      networkType?: string;
      cniVersion?: string;
    };
    externalAccess: {
      apiIP: string;
      ingressIP: string;
      publishAppRoutes?: boolean;
    };
    bastion?: {
      graphical?: boolean;
      username?: string;
    };
  };
  status: {
    phase: string;
    // Release spec.ocpVersion resolved to ("4.22" -> "4.22.22"), set at GenerateISO.
    releaseVersion?: string;
    // "bastion" when the external addresses live on the bastion VM; empty
    // for older clusters, where they belong to the HAProxy pods.
    loadBalancer?: string;
    // The bastion's cluster-network address, set once the bastion exists.
    mcsRelayIP?: string;
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
    hostAPIRoute?: string;
    // Host-route URLs (externalAccess.hostRoutes), certificates signed by
    // the per-cluster CA served at /external-ca.
    hostConsoleURL?: string;
    hostAPIURL?: string;
    // Applications published under the host's *.apps (per-application routes).
    appRoutes?: Array<{ source: string; url: string }>;
    conditions?: Array<{ type: string; status: string; reason: string; message: string }>;
    installConfig?: string;
    agentConfig?: string;
  };
}

const PHASE_COLORS: Record<string, 'green' | 'blue' | 'orange' | 'red' | 'grey'> = {
  Ready: 'green',
  Failed: 'red',
  Master: 'grey',
};

const PHASE_VARIANT: Record<string, 'success' | 'info' | 'warning' | 'danger' | 'pending'> = {
  Ready: 'success',
  Failed: 'danger',
};

// Optional post-install features, reported as status conditions.
const FEATURE_CONDITIONS: Array<{ type: string; label: string }> = [
  { type: 'NetworkReady', label: 'Network self-test' },
  { type: 'StorageReady', label: 'Nested storage' },
  { type: 'VirtualizationReady', label: 'OpenShift Virtualization' },
  { type: 'HostRoutesReady', label: 'Host routes' },
  { type: 'Isolated', label: 'Isolated from its master' },
];

const ClusterDetailPage: FC = () => {
  const { name } = useParams<{ name: string }>();
  const { t } = useTranslation(I18N);
  const [cluster, setCluster] = useState<ClusterDetail>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string>();
  const [showDelete, setShowDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [kubeadminPwd, setKubeadminPwd] = useState<string>();
  const [actionError, setActionError] = useState<string | undefined>();
  // Controlled ExpandableSection needs an onToggle, otherwise clicking does
  // nothing: once the cluster was Ready the log was collapsed forever and
  // looked empty.
  const [logExpanded, setLogExpanded] = useState<boolean | undefined>();

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
      setActionError(undefined);
    } catch (e) {
      // Never swallow this: a failure here looks to the user like a dead
      // button. The namespace bug behind it (secrets live in the CLUSTER
      // namespace, not the CR's) produced a 500 that nothing surfaced.
      setActionError(
        t('Could not download kubeconfig: {{msg}}', { msg: (e as Error)?.message || String(e) }),
      );
    }
  };

  const handleDownloadCA = async () => {
    if (!name) return;
    try {
      const data = (await fetchExternalCA(name)) as { caCert: string };
      const blob = new Blob([data.caCert + '\n'], { type: 'application/x-pem-file' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `${name}-ca.crt`;
      a.click();
      URL.revokeObjectURL(url);
      setActionError(undefined);
    } catch (e) {
      setActionError(t('Could not download the CA: {{msg}}', { msg: (e as Error)?.message || String(e) }));
    }
  };

  const handleShowKubeadmin = async () => {
    if (!name) return;
    try {
      const data = (await fetchKubeadmin(name)) as { password: string };
      setKubeadminPwd(data.password);
      setActionError(undefined);
    } catch (e) {
      setActionError(
        t('Could not read kubeadmin password: {{msg}}', { msg: (e as Error)?.message || String(e) }),
      );
    }
  };

  if (loading) return <Spinner size="lg" />;
  if (error) return <Alert variant="danger" isInline title={error} />;
  if (!cluster) return <Alert variant="warning" isInline title={t('Cluster not found')} />;

  const { spec, status } = cluster;
  const isReady = status.phase === 'Ready';
  const fqdn = spec.source?.internalName
    ? `${spec.source.internalName}.${spec.source.internalBaseDomain}`
    : `${spec.clusterName}.${spec.baseDomain}`;
  // The bastion is up from the MCSRelay phase on, long before Ready, and is
  // the place to debug a stuck install from, so show how to reach it early.
  // The cloud-init key lands on user core (as on the RHCOS nodes); the image
  // defaults (cloud-user, centos) have no key, which is easy to trip over.
  const bastionOnVLAN = status.loadBalancer === 'bastion';
  const bastionHost = bastionOnVLAN ? spec.externalAccess?.apiIP : status.mcsRelayIP;
  const desktopUser = spec.bastion?.graphical ? spec.bastion.username : undefined;

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
            {t('Shift-on-Shift Virt')}
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
              {spec.source?.master && (
                <SplitItem>
                  <Label color="purple">{t('Clone of {{m}}', { m: spec.source.master })}</Label>
                </SplitItem>
              )}
            </Split>
          </StackItem>
          {status.phase === 'Master' && (
            <StackItem>
              <Alert variant="info" isInline title={t('This cluster is a master')}>
                {t('It is shut down and kept as the template for Express clones (Masters tab). Clone it from the quick cluster card on the Install tab. It cannot be deleted while it has clones.')}
              </Alert>
            </StackItem>
          )}

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
                isExpanded={logExpanded ?? !isReady}
                onToggle={(_e, expanded) => setLogExpanded(expanded)}
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

          {(status.installConfig || status.agentConfig) && (
            <StackItem>
              <ConfigViewer
                installConfig={status.installConfig}
                agentConfig={status.agentConfig}
              />
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
                  <DescriptionListTerm>{t('API Endpoint (cluster DNS)')}</DescriptionListTerm>
                  <DescriptionListDescription>
                    <ClipboardCopy isReadOnly variant="inline-compact">
                      {status.apiEndpoint}
                    </ClipboardCopy>
                    <HelperText>
                      <HelperTextItem variant="indeterminate">
                        {t('Needs api.{{fqdn}} in your DNS, pointing at the external VIP.', {
                          fqdn,
                        })}
                      </HelperTextItem>
                    </HelperText>
                  </DescriptionListDescription>
                </DescriptionListGroup>
                {status.hostConsoleURL && status.hostAPIURL && (
                  <>
                    <DescriptionListGroup>
                      <DescriptionListTerm>{t('Console (host route)')}</DescriptionListTerm>
                      <DescriptionListDescription>
                        <a href={status.hostConsoleURL} target="_blank" rel="noopener noreferrer">
                          {status.hostConsoleURL}
                        </a>
                        <HelperText>
                          <HelperTextItem variant="indeterminate">
                            {t(
                              'No DNS needed. The certificate is signed by this cluster\u2019s own CA: download it and trust it, or your browser shows a warning.',
                            )}
                          </HelperTextItem>
                        </HelperText>
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    <DescriptionListGroup>
                      <DescriptionListTerm>{t('API Endpoint (host route)')}</DescriptionListTerm>
                      <DescriptionListDescription>
                        <ClipboardCopy isReadOnly variant="inline-compact">
                          {status.hostAPIURL}
                        </ClipboardCopy>
                        <Button variant="link" isInline onClick={handleDownloadCA}>
                          {t('Download CA certificate')}
                        </Button>
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    <DescriptionListGroup>
                      <DescriptionListTerm>{t('Login command')}</DescriptionListTerm>
                      <DescriptionListDescription>
                        <ClipboardCopy isReadOnly variant="expansion" isCode>
                          {`oc login ${status.hostAPIURL} -u kubeadmin -p ${
                            kubeadminPwd || '<click \u201cCopy Kubeadmin Password\u201d>'
                          } --certificate-authority=${name}-ca.crt`}
                        </ClipboardCopy>
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  </>
                )}
                {status.hostConsoleURL && (
                  <DescriptionListGroup>
                    <DescriptionListTerm>{t('Published applications')}</DescriptionListTerm>
                    <DescriptionListDescription>
                      {(status.appRoutes || []).map((a) => (
                        <div key={a.source}>
                          <a href={a.url} target="_blank" rel="noopener noreferrer">{a.url}</a> ({a.source})
                        </div>
                      ))}
                      {spec.externalAccess?.publishAppRoutes && (
                        <Label color="blue">{t('All application routes are published automatically')}</Label>
                      )}
                      <HelperText>
                        <HelperTextItem variant="indeterminate">
                          {spec.externalAccess?.publishAppRoutes
                            ? t('Every route outside openshift-* and kube-* appears here within about two minutes. Opt one out with the annotation below set to false.')
                            : t('Publish a route of the nested cluster under this cluster\u2019s *.apps (its trusted certificate, no DNS): annotate it, and it appears here within about two minutes. Edge and reencrypt routes only; the hop from this cluster\u2019s router to the nested cluster is plain HTTP.')}
                        </HelperTextItem>
                      </HelperText>
                      <ClipboardCopy isReadOnly isCode variant="inline-compact">
                        {'oc annotate route <name> -n <namespace> sos.octools.io/publish-host-route=true'}
                      </ClipboardCopy>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                )}
                {status.hostAPIRoute && !status.hostAPIURL && (
                  <>
                    <DescriptionListGroup>
                      <DescriptionListTerm>{t('API Endpoint (host route)')}</DescriptionListTerm>
                      <DescriptionListDescription>
                        <ClipboardCopy isReadOnly variant="inline-compact">
                          {status.hostAPIRoute}
                        </ClipboardCopy>
                        <HelperText>
                          <HelperTextItem variant="indeterminate">
                            {t(
                              'Served through this cluster\u2019s router \u2014 no extra DNS needed. The nested certificate does not carry this hostname, so --insecure-skip-tls-verify is required.',
                            )}
                          </HelperTextItem>
                        </HelperText>
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    <DescriptionListGroup>
                      <DescriptionListTerm>{t('Login command')}</DescriptionListTerm>
                      <DescriptionListDescription>
                        <ClipboardCopy isReadOnly variant="expansion" isCode>
                          {`oc login ${status.hostAPIRoute} -u kubeadmin -p ${
                            kubeadminPwd || '<click \u201cCopy Kubeadmin Password\u201d>'
                          } --insecure-skip-tls-verify`}
                        </ClipboardCopy>
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  </>
                )}
                <DescriptionListGroup>
                  <DescriptionListTerm>{t('Ingress Domain')}</DescriptionListTerm>
                  <DescriptionListDescription>{status.ingressDomain}</DescriptionListDescription>
                </DescriptionListGroup>
                <DescriptionListGroup>
                  <DescriptionListTerm>{t('OpenShift version')}</DescriptionListTerm>
                  <DescriptionListDescription>
                    {status.releaseVersion || cluster.spec.ocpVersion}
                  </DescriptionListDescription>
                </DescriptionListGroup>
                <DescriptionListGroup>
                  <DescriptionListTerm>{t('Network plugin')}</DescriptionListTerm>
                  <DescriptionListDescription>
                    {cluster.spec.network?.networkType && cluster.spec.network.networkType !== 'OVNKubernetes'
                      ? [cluster.spec.network.networkType, cluster.spec.network.cniVersion].filter(Boolean).join(' ')
                      : 'OVN-Kubernetes'}
                  </DescriptionListDescription>
                </DescriptionListGroup>
                {FEATURE_CONDITIONS.map(({ type, label }) => {
                  const c = status.conditions?.find((x) => x.type === type);
                  if (!c) return null;
                  const ok = c.status === 'True';
                  return (
                    <DescriptionListGroup key={type}>
                      <DescriptionListTerm>{t(label)}</DescriptionListTerm>
                      <DescriptionListDescription>
                        <Label color={ok ? 'green' : 'orange'}>{ok ? t('Ready') : t('Not ready')}</Label>{' '}
                        {c.message}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  );
                })}
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
              {actionError && (
                <Alert variant="danger" isInline title={actionError} className="sos-action-error" />
              )}
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
              {status.mcsRelayIP && bastionHost && (
                <DescriptionListGroup>
                  <DescriptionListTerm>{t('Bastion SSH')}</DescriptionListTerm>
                  <DescriptionListDescription>
                    <ClipboardCopy isReadOnly variant="inline-compact" isCode>
                      {`ssh core@${bastionHost}`}
                    </ClipboardCopy>
                    <HelperText>
                      <HelperTextItem>
                        {t(
                          'Log in as core with the SSH key you gave the wizard; core has passwordless sudo. The image\u2019s default users (cloud-user, centos) have no key.',
                        )}
                      </HelperTextItem>
                      {bastionOnVLAN ? (
                        <HelperTextItem variant="indeterminate">
                          {t('The bastion also answers on {{ip}}.', {
                            ip: spec.externalAccess.ingressIP,
                          })}
                        </HelperTextItem>
                      ) : (
                        <HelperTextItem variant="indeterminate">
                          {t(
                            'This is the bastion\u2019s cluster network address: reachable only from the cluster network, for example from a pod in {{namespace}}.',
                            { namespace: spec.namespace || `${spec.clusterName}-ns` },
                          )}
                        </HelperTextItem>
                      )}
                      {desktopUser && (
                        <HelperTextItem variant="indeterminate">
                          {t(
                            'Desktop: log in as {{user}} in the console of VM {{vm}}.',
                            { user: desktopUser, vm: `${spec.clusterName}-bastion` },
                          )}
                        </HelperTextItem>
                      )}
                    </HelperText>
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
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
