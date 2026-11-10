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
  Radio,
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
  // Access path; starts from the lab default (publishHostRoutes).
  const [hostRoutes, setHostRoutes] = useState<boolean | undefined>(undefined);
  const [publishApps, setPublishApps] = useState<boolean | undefined>(undefined);
  // Express: clone from a master instead of installing ('' = fresh install).
  const [masters, setMasters] = useState<MasterItem[]>([]);
  const [master, setMaster] = useState('');
  useEffect(() => {
    fetchMasters().then(setMasters).catch(() => setMasters([]));
  }, []);
  useEffect(() => {
    fetchLabDefaults()
      .then((d) => {
        setDefaults(d);
        setGraphical(Boolean(d.settings?.bastionGraphical));
        setHostRoutes(Boolean(d.settings?.publishHostRoutes));
        setPublishApps(Boolean(d.settings?.publishHostRoutes && d.settings?.publishAppRoutes));
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
      fetchQuickPlan(name, master)
        .then((p) => {
          setPlan(p);
          setPlanError(undefined);
          if (!name && p.spec?.clusterName) setName(p.spec.clusterName);
        })
        .catch((e) => setPlanError(fetchErrorText(e)));
    }, name ? 400 : 0);
    return () => clearTimeout(timer.current);
  }, [name, master]);

  const deploy = async () => {
    setDeploying(true);
    setDeployError(undefined);
    try {
      await createQuickCluster(name.trim(), {
        // Only an edited key is sent; otherwise the server uses the profile key.
        ...(keyChanged && sshKey.trim() ? { sshPublicKey: sshKey.trim() } : {}),
        ...(graphical !== undefined ? { graphical } : {}),
        ...(hostRoutes !== undefined && !master ? { hostRoutes } : {}),
        // Clones always use host routes, so the choice applies to them too.
        ...(publishApps !== undefined && (hostRoutes || master) ? { publishAppRoutes: publishApps } : {}),
        ...(master ? { master } : {}),
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
        {masters.length > 0 && (
          <FormGroup label={t('How to create it')} fieldId="quick-source">
            <FormSelect id="quick-source" value={master} onChange={(_e, v) => setMaster(v)}>
              <FormSelectOption value="" label={t('Fresh install (about 60 minutes)')} />
              {masters.map((m) => (
                <FormSelectOption key={m.name} value={m.name} label={t('Clone from master {{m}} (usable in a few minutes)', { m: masterLabel(m) })} />
              ))}
            </FormSelect>
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  {master
                    ? t('Shift-on-Shift Express: copied from the master, then given its own credentials. Usable within minutes; isolated from the master and other clones about 15 minutes later. Clones always use hosting cluster routes and keep the master\'s internal name, sizing and OpenShift release.')
                    : t('Installs OpenShift from scratch with the lab defaults.')}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
        )}
        {!master && (
        <FormGroup role="radiogroup" isInline={false} label={t('How people reach the console and API')} fieldId="quick-access">
          <Radio
            id="quick-access-dns"
            name="quick-access"
            isChecked={hostRoutes === false}
            onChange={() => setHostRoutes(false)}
            label={t('External DNS')}
            description={
              s
                ? t('You add two DNS records: api.{{fqdn}} → {{api}} and *.apps.{{fqdn}} → {{ingress}}. Everything works natively with the cluster\'s own certificates: console, login, API and all applications.', {
                    fqdn: `${s.clusterName}.${s.baseDomain}`,
                    api: s.externalAccess?.apiIP,
                    ingress: s.externalAccess?.ingressIP,
                  })
                : t('You add DNS records for the cluster\'s API and *.apps names. Everything works natively with the cluster\'s own certificates.')
            }
          />
          <Radio
            id="quick-access-routes"
            name="quick-access"
            isChecked={hostRoutes === true}
            onChange={() => setHostRoutes(true)}
            label={t('Hosting cluster routes (no DNS needed)')}
            description={t('Console, login and API are published as routes on this cluster, under its *.apps domain, so they work right away. Certificates come from a per-cluster CA you download from the cluster page (browsers warn until you trust it). Console and login then always use these names; your own applications in the nested cluster still need DNS. The API also keeps working through DNS if you add it later.')}
          />
        </FormGroup>
        )}
        {(hostRoutes || master) && (
          <Checkbox
            id="quick-publish-apps"
            isChecked={Boolean(publishApps)}
            onChange={(_e, v) => setPublishApps(v)}
            label={t('Also publish all application routes')}
            description={t('Every route created in the cluster (outside openshift-* and kube-*) gets an address under this cluster\'s *.apps with its trusted certificate. Opt a route out with sos.octools.io/publish-host-route=false.')}
          />
        )}
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
