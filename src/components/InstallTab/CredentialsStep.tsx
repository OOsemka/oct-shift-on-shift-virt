import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Form,
  FormGroup,
  FormHelperText,
  HelperText,
  HelperTextItem,
  TextArea,
  TextInput,
  Checkbox,
} from '@patternfly/react-core';
import { ClusterConfig } from './types';
import { validatePullSecret, validateSSHKey } from '../../utils/validation';
import { LabDefaults } from '../../utils/labdefaults';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
  /** Lab defaults status; null while loading or unavailable. */
  labDefaults?: LabDefaults | null;
}

const CredentialsStep: FC<Props> = ({ config, onChange, labDefaults }) => {
  const { t } = useTranslation(I18N);

  const pullErr = config.pullSecret ? validatePullSecret(config.pullSecret) : undefined;
  const sshErr = config.sshKey ? validateSSHKey(config.sshKey) : undefined;

  return (
    <Form className="sos-form">
      <FormGroup label={t('Pull Secret')} isRequired={!labDefaults?.pullSecretSet} fieldId="pull-secret">
        <TextArea
          id="pull-secret"
          value={config.pullSecret}
          onChange={(_e, val) => onChange({ pullSecret: val })}
          validated={pullErr ? 'error' : 'default'}
          rows={6}
          resizeOrientation="vertical"
          placeholder={
            labDefaults?.pullSecretSet
              ? t('Leave empty to use the lab default pull secret')
              : '{"auths":{"cloud.openshift.com":{"auth":"..."},...}}'
          }
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={pullErr ? 'error' : 'default'}>
              {pullErr || (
                <>
                  {labDefaults?.pullSecretSet
                    ? t('Leave empty to use the lab default, or paste a pull secret for this cluster only. ')
                    : t('Paste your Red Hat pull secret JSON. ')}
                  <a
                    href="https://console.redhat.com/openshift/install/metal/agent-based"
                    target="_blank"
                    rel="noopener noreferrer"
                  >
                    {t('Get your pull secret from Red Hat')}
                  </a>
                </>
              )}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('SSH Public Key')} fieldId="ssh-key">
        <TextArea
          id="ssh-key"
          value={config.sshKey}
          onChange={(_e, val) => onChange({ sshKey: val })}
          validated={sshErr ? 'error' : 'default'}
          rows={3}
          resizeOrientation="vertical"
          placeholder={labDefaults?.sshKeyFingerprint ? t('Leave empty to use the lab default key') : 'ssh-ed25519 AAAA...'}
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={sshErr ? 'error' : 'default'}>
              {sshErr ||
                (labDefaults?.sshKeyFingerprint
                  ? t('Leave empty to use the lab default key ({{fp}}).', { fp: labDefaults.sshKeyFingerprint })
                  : t('Paste your SSH public key for node access'))}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <Alert
        variant="info"
        isInline
        isPlain
        title={t('Bastion access: passwordless SSH')}
        className="sos-bastion-access"
      >
        <p>
          {config.externalAccess.apiIP
            ? t('Log in with ssh core@{{ip}} (the external API address; the Ingress address works too).', { ip: config.externalAccess.apiIP })
            : t('Log in with ssh core@<external API address>.')}{' '}
          {config.sshKey.trim()
            ? t('Your key above is authorized.')
            : labDefaults?.sshKeyFingerprint
              ? t('The lab default key is authorized ({{fp}}).', { fp: labDefaults.sshKeyFingerprint })
              : t('Paste an SSH public key above, or store one in Lab Defaults: without a key there is no SSH access for you.')}
        </p>
        <p>{t('SSH accepts keys only, never passwords. The cluster page shows the command once the bastion is up.')}</p>
      </Alert>

      <FormGroup label={t('Graphical bastion (optional)')} fieldId="bastion-graphical">
        <Checkbox
          id="bastion-graphical"
          isChecked={config.bastion.graphical}
          onChange={(_e, checked) =>
            onChange({ bastion: { ...config.bastion, graphical: checked } })
          }
          label={t('Install a desktop on the bastion VM (only needed for a browser on the bastion)')}
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem>
              {t(
                'The bastion runs the nested cluster DNS, so a browser on it reaches the nested console directly. Open it from Virtualization → VirtualMachines → <cluster>-bastion → Console. Adds ~2 vCPU / 4 GiB to the bastion.',
              )}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      {config.bastion.graphical && (
        <>
          <FormGroup label={t('Desktop username')} fieldId="bastion-user">
            <TextInput
              id="bastion-user"
              value={config.bastion.username}
              onChange={(_e, val) =>
                onChange({ bastion: { ...config.bastion, username: val } })
              }
              placeholder="core"
            />
          </FormGroup>
          <FormGroup label={t('Desktop password')} fieldId="bastion-password">
            <TextInput
              id="bastion-password"
              type="password"
              value={config.bastion.password}
              onChange={(_e, val) =>
                onChange({ bastion: { ...config.bastion, password: val } })
              }
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  {t(
                    'Only for the desktop login in the VM console (it never works over SSH). Stored in a Secret and cleared from the cluster resource; leave blank to have one generated.',
                  )}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
        </>
      )}
    </Form>
  );
};

export default CredentialsStep;
