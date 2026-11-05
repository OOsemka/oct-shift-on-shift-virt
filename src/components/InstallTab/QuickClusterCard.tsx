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
  Checkbox,
  Flex,
  FlexItem,
  FormGroup,
  FormHelperText,
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

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  /** Switch the page to the Lab Defaults tab. */
  onOpenDefaults: () => void;
}

/** One-button compact 3-node cluster from the lab defaults. */
const QuickClusterCard: FC<Props> = ({ onOpenDefaults }) => {
  const { t } = useTranslation(I18N);
  const navigate = useNavigate();
  const [name, setName] = useState('');
  const [plan, setPlan] = useState<QuickPlan | null>(null);
  const [planError, setPlanError] = useState<string>();
  const [deploying, setDeploying] = useState(false);
  const [deployError, setDeployError] = useState<string>();
  const timer = useRef<ReturnType<typeof setTimeout>>();
  const [defaults, setDefaults] = useState<LabDefaults | null>(null);
  // Per-cluster choices; the desktop starts from the lab default.
  const [graphical, setGraphical] = useState<boolean | undefined>(undefined);
  const [sshKey, setSshKey] = useState('');
  const [desktopUser, setDesktopUser] = useState('core');
  const [desktopPassword, setDesktopPassword] = useState('');
  useEffect(() => {
    fetchLabDefaults()
      .then((d) => {
        setDefaults(d);
        setGraphical(Boolean(d.settings?.bastionGraphical));
        // Pre-filled with the profile key; edit it to use another key for this cluster.
        setSshKey(d.sshPublicKey || '');
      })
      .catch(() => setDefaults(null));
  }, []);
  const keyChanged = sshKey.trim() !== (defaults?.sshPublicKey || '').trim();
  const pwErr = desktopPassword && desktopPassword.length < 8 ? t('At least 8 characters, or leave empty to have one generated.') : undefined;
  const sshErr = sshKey.trim() ? sshKeyProblem(sshKey) : undefined;

  // The server decides everything but the name; ask it for the exact plan.
  useEffect(() => {
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      fetchQuickPlan(name)
        .then((p) => {
          setPlan(p);
          setPlanError(undefined);
          if (!name && p.spec?.clusterName) setName(p.spec.clusterName);
        })
        .catch((e) => setPlanError(fetchErrorText(e)));
    }, name ? 400 : 0);
    return () => clearTimeout(timer.current);
  }, [name]);

  const deploy = async () => {
    setDeploying(true);
    setDeployError(undefined);
    try {
      await createQuickCluster(name.trim(), {
        // Only an edited key is sent; otherwise the server uses the profile key.
        ...(keyChanged && sshKey.trim() ? { sshPublicKey: sshKey.trim() } : {}),
        ...(graphical !== undefined ? { graphical } : {}),
        ...(graphical ? { desktopUsername: desktopUser.trim(), desktopPassword } : {}),
      });
      navigate(`/community-tools/compute/shift-on-shift-virt/cluster/${encodeURIComponent(name.trim())}`);
    } catch (e) {
      setDeployError(fetchErrorText(e));
    } finally {
      setDeploying(false);
    }
  };

  const s = plan?.spec;
  const ready = Boolean(plan && plan.problems.length === 0 && name.trim() && !sshErr && !pwErr);
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
                {t('3 nodes × {{cores}} vCPU / {{mem}} GiB / {{disk}} GiB · OpenShift {{ver}} · {{domain}} · API {{api}} · Ingress {{ingress}} · pool {{pool}}', {
                  cores: String(s.masters?.cpuCores),
                  mem: String(s.masters?.memoryGi),
                  disk: String(s.masters?.diskGi),
                  ver: s.ocpVersion,
                  domain: s.baseDomain,
                  api: s.externalAccess?.apiIP,
                  ingress: s.externalAccess?.ingressIP,
                  pool: s.externalAccess?.ipPool,
                })}
                {s.virtualization?.enabled
                  ? ` · ${t('nested storage + OpenShift Virtualization')}`
                  : s.storage?.enabled
                    ? ` · ${t('nested storage (KubeVirt CSI)')}`
                    : ''}
                <br />
                {sshKey.trim() && !sshErr
                  ? t('SSH: ssh core@{{api}} with the key below, no password.', { api: s.externalAccess?.apiIP })
                  : t('No SSH key: add one below or in Lab Defaults to be able to SSH to the bastion.')}
              </span>
            )}
          </FlexItem>
        </Flex>
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
        <Checkbox
          id="quick-graphical"
          isChecked={Boolean(graphical)}
          onChange={(_e, v) => setGraphical(v)}
          label={t('Install a desktop on the bastion (adds 2 vCPU / 4 GiB)')}
        />
        {graphical && (
          <Flex spaceItems={{ default: 'spaceItemsMd' }}>
            <FlexItem>
              <FormGroup label={t('Desktop username')} fieldId="quick-desktop-user">
                <TextInput id="quick-desktop-user" value={desktopUser} onChange={(_e, v) => setDesktopUser(v)} />
              </FormGroup>
            </FlexItem>
            <FlexItem>
              <FormGroup label={t('Desktop password')} fieldId="quick-desktop-pw">
                <TextInput
                  id="quick-desktop-pw"
                  type="password"
                  value={desktopPassword}
                  onChange={(_e, v) => setDesktopPassword(v)}
                  validated={pwErr ? 'error' : 'default'}
                  placeholder={t('Leave empty to generate')}
                />
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem variant={pwErr ? 'error' : 'default'}>
                      {pwErr || t('Only for the desktop login in the VM console, never over SSH. Stored in a Secret.')}
                    </HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FormGroup>
            </FlexItem>
          </Flex>
        )}
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
        <span className="sos-quick-hint">{t('Or use the wizard below for anything else.')}</span>
      </CardFooter>
    </Card>
  );
};

export default QuickClusterCard;
