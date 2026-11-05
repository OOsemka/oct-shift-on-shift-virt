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
  Flex,
  FlexItem,
  FormGroup,
  TextInput,
} from '@patternfly/react-core';
import {
  createQuickCluster,
  fetchErrorText,
  fetchLabDefaults,
  fetchQuickPlan,
  LabDefaults,
  QuickPlan,
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
  useEffect(() => {
    fetchLabDefaults().then(setDefaults).catch(() => setDefaults(null));
  }, []);

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
      await createQuickCluster(name.trim());
      navigate(`/community-tools/compute/shift-on-shift-virt/cluster/${encodeURIComponent(name.trim())}`);
    } catch (e) {
      setDeployError(fetchErrorText(e));
    } finally {
      setDeploying(false);
    }
  };

  const s = plan?.spec;
  const ready = Boolean(plan && plan.problems.length === 0 && name.trim());
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
                <br />
                {defaults?.sshKeyFingerprint
                  ? t('SSH: ssh core@{{api}} with the lab default key ({{fp}}), no password.', {
                      api: s.externalAccess?.apiIP,
                      fp: defaults.sshKeyFingerprint,
                    })
                  : t('No lab default SSH key: you will not be able to SSH to the bastion.')}
              </span>
            )}
          </FlexItem>
        </Flex>
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
