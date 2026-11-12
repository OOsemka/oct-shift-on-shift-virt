import React, { FC, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom-v5-compat';
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardFooter,
  CardTitle,
  Form,
  Flex,
  FlexItem,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  TextArea,
  TextInput,
} from '@patternfly/react-core';
import {
  createQuickCluster,
  fetchErrorText,
  fetchLabDefaults,
  fetchQuickPlan,
  LabDefaults,
  QuickPlan,
  sshKeyProblem,
} from '../../utils/labdefaults';
import { fetchMasters, masterLabel, MasterItem } from '../../utils/masters';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  /** Switch the page to the Lab Defaults tab. */
  onOpenDefaults: () => void;
  /** Changes when the Install tab is shown again: reload defaults, masters and the plan. */
  refreshKey?: number;
}

/** One-button compact 3-node cluster from the lab defaults. */
const QuickClusterCard: FC<Props> = ({ onOpenDefaults, refreshKey = 0 }) => {
  const { t } = useTranslation(I18N);
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [plan, setPlan] = useState<QuickPlan | null>(null);
  const [planError, setPlanError] = useState<string>();
  const [deploying, setDeploying] = useState(false);
  const [deployError, setDeployError] = useState<string>();
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const [defaults, setDefaults] = useState<LabDefaults | null>(null);
  // The quick card is one button: everything else (access path, published
  // applications, bastion desktop) comes from the Lab Defaults; the full
  // wizard offers every choice.
  const [sshKey, setSshKey] = useState('');
  // Express: clone from a master instead of installing ('' = fresh install).
  const [masters, setMasters] = useState<MasterItem[]>([]);
  const [master, setMaster] = useState('');
  useEffect(() => {
    fetchMasters()
      .then(setMasters)
      .catch(() => setMasters([]));
  }, [refreshKey]);
  // The profile key the SSH field was last filled with: a reload replaces the
  // field only if the user has not edited it.
  const profileKey = useRef('');
  useEffect(() => {
    fetchLabDefaults()
      .then((d) => {
        setDefaults(d);
        // Pre-filled with the profile key; edit it to use another key for this cluster.
        const next = d.sshPublicKey || '';
        setSshKey((cur) => (cur.trim() === profileKey.current.trim() ? next : cur));
        profileKey.current = next;
      })
      .catch(() => setDefaults(null));
  }, [refreshKey]);
  const keyChanged = sshKey.trim() !== (defaults?.sshPublicKey || '').trim();
  const sshErr = sshKey.trim() ? sshKeyProblem(sshKey) : undefined;

  // The server decides everything but the name; ask it for the exact plan.
  useEffect(() => {
    clearTimeout(timer.current);
    timer.current = setTimeout(
      () => {
        fetchQuickPlan(name, master)
          .then((p) => {
            setPlan(p);
            setPlanError(undefined);
            if (!name && p.spec?.clusterName) setName(p.spec.clusterName);
          })
          .catch((e) => setPlanError(fetchErrorText(e)));
      },
      name ? 400 : 0,
    );
    return () => clearTimeout(timer.current);
  }, [name, master, refreshKey]);

  const deploy = async () => {
    setDeploying(true);
    setDeployError(undefined);
    try {
      await createQuickCluster(name.trim(), {
        // Only an edited key is sent; otherwise the server uses the profile key.
        ...(keyChanged && sshKey.trim() ? { sshPublicKey: sshKey.trim() } : {}),
        ...(master ? { master } : {}),
      });
      navigate(`/community-tools/compute/shift-on-shift-virt/cluster/${encodeURIComponent(name.trim())}`);
    } catch (e) {
      setDeployError(fetchErrorText(e));
    } finally {
      setDeploying(false);
    }
  };

  const s = plan?.spec;
  const ready = Boolean(plan && plan.problems.length === 0 && name.trim() && !sshErr);
  return (
    <Card className="sos-quick-card" isCompact>
      <CardTitle>{t('Quick compact cluster')}</CardTitle>
      <CardBody>
        <Flex alignItems={{ default: 'alignItemsFlexEnd' }} spaceItems={{ default: 'spaceItemsMd' }}>
          <FlexItem>
            <FormGroup label={t('Cluster name')} fieldId="quick-name">
              <TextInput id="quick-name" value={name} onChange={(_e, v) => setName(v.trim().toLowerCase())} />
            </FormGroup>
          </FlexItem>
          <FlexItem grow={{ default: 'grow' }}>
            {s && plan?.problems.length === 0 && (
              <span>
                {t(
                  '3 nodes × {{cores}} vCPU / {{mem}} GiB / {{disk}} GiB · OpenShift {{ver}} · {{domain}} · API {{api}} · Ingress {{ingress}} · pool {{pool}}',
                  {
                    cores: String(s.masters?.cpuCores),
                    mem: String(s.masters?.memoryGi),
                    disk: String(s.masters?.diskGi),
                    ver: s.ocpVersion,
                    domain: s.baseDomain,
                    api: s.externalAccess?.apiIP,
                    ingress: s.externalAccess?.ingressIP,
                    pool: s.externalAccess?.ipPool,
                  },
                )}
                {s.virtualization?.enabled
                  ? ` · ${t('nested storage + OpenShift Virtualization')}`
                  : s.storage?.enabled
                    ? ` · ${t('nested storage (KubeVirt CSI)')}`
                    : ''}
                {s.externalAccess?.hostRoutes
                  ? ` · ${t('console and API through host routes')}`
                  : ` · ${t('needs DNS records')}`}
                {s.externalAccess?.publishAppRoutes ? ` · ${t('applications published')}` : ''}
                {s.bastion?.graphical ? ` · ${t('bastion desktop')}` : ''}
                <br />
                {sshKey.trim() && !sshErr
                  ? t('SSH: ssh core@{{api}} with the key below, no password.', { api: s.externalAccess?.apiIP })
                  : t('No SSH key: add one below or in Lab Defaults to be able to SSH to the bastion.')}
              </span>
            )}
          </FlexItem>
        </Flex>
        <Form>
          <FormGroup label={t('SSH public key')} fieldId="quick-ssh">
            <TextArea
              id="quick-ssh"
              value={sshKey}
              onChange={(_e, v) => setSshKey(v)}
              rows={2}
              placeholder="ssh-ed25519 AAAA... user@host"
              validated={sshErr ? 'error' : 'default'}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem variant={sshErr ? 'error' : 'default'}>
                  {sshErr ||
                    (keyChanged
                      ? t('This key is used for this cluster only; the lab default stays unchanged.')
                      : t('From the lab defaults. Edit it to use another key for this cluster only.'))}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          {masters.length > 0 && (
            <FormGroup label={t('How to create it')} fieldId="quick-source">
              <FormSelect id="quick-source" value={master} onChange={(_e, v) => setMaster(v)}>
                <FormSelectOption value="" label={t('Fresh install (about 60 minutes)')} />
                {masters.map((m) => (
                  <FormSelectOption
                    key={m.name}
                    value={m.name}
                    label={t('Clone from master {{m}} (usable in a few minutes)', { m: masterLabel(m) })}
                  />
                ))}
              </FormSelect>
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>
                    {master
                      ? t(
                          "Shift-on-Shift Express: copied from the master and given its own credentials, ready in minutes. It keeps the master's sizing and OpenShift release.",
                        )
                      : t('Installs OpenShift from scratch with the lab defaults.')}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FormGroup>
          )}
        </Form>
        {plan && plan.problems.length > 0 && (
          <Alert
            variant="warning"
            isInline
            isPlain
            title={t('The quick cluster needs a few lab defaults first')}
            actionLinks={
              <Button variant="link" isInline onClick={onOpenDefaults}>
                {t('Open Lab Defaults')}
              </Button>
            }
          >
            <ul>
              {plan.problems.map((p) => (
                <li key={p}>{p}</li>
              ))}
            </ul>
          </Alert>
        )}
        {planError && (
          <Alert variant="danger" isInline isPlain title={t('Could not prepare the quick cluster')}>
            {planError}
          </Alert>
        )}
        {deployError && (
          <Alert variant="danger" isInline title={t('Deploy failed')}>
            {deployError}
          </Alert>
        )}
      </CardBody>
      <CardFooter>
        <Button variant="primary" onClick={deploy} isLoading={deploying} isDisabled={!ready || deploying}>
          {t('Deploy')}
        </Button>{' '}
        <span className="sos-quick-hint">
          {t('Settings come from the Lab Defaults. Use the wizard below for anything else.')}
        </span>
      </CardFooter>
    </Card>
  );
};

export default QuickClusterCard;
