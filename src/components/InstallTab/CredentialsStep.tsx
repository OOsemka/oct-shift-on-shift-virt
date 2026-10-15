import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
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

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
}

const CredentialsStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);

  const pullErr = config.pullSecret ? validatePullSecret(config.pullSecret) : undefined;
  const sshErr = config.sshKey ? validateSSHKey(config.sshKey) : undefined;

  return (
    <Form className="sos-form">
      <FormGroup label={t('Pull Secret')} isRequired fieldId="pull-secret">
        <TextArea
          id="pull-secret"
          value={config.pullSecret}
          onChange={(_e, val) => onChange({ pullSecret: val })}
          validated={pullErr ? 'error' : 'default'}
          rows={6}
          resizeOrientation="vertical"
          placeholder='{"auths":{"cloud.openshift.com":{"auth":"..."},...}}'
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={pullErr ? 'error' : 'default'}>
              {pullErr || (
                <>
                  {t('Paste your Red Hat pull secret JSON. ')}
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

      <FormGroup label={t('SSH Public Key')} isRequired fieldId="ssh-key">
        <TextArea
          id="ssh-key"
          value={config.sshKey}
          onChange={(_e, val) => onChange({ sshKey: val })}
          validated={sshErr ? 'error' : 'default'}
          rows={3}
          resizeOrientation="vertical"
          placeholder="ssh-ed25519 AAAA..."
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={sshErr ? 'error' : 'default'}>
              {sshErr || t('Paste your SSH public key for node access')}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </FormGroup>

      <FormGroup label={t('Graphical bastion')} fieldId="bastion-graphical">
        <Checkbox
          id="bastion-graphical"
          isChecked={config.bastion.graphical}
          onChange={(_e, checked) =>
            onChange({ bastion: { ...config.bastion, graphical: checked } })
          }
          label={t('Install a desktop on the bastion VM')}
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
          <FormGroup label={t('Bastion username')} fieldId="bastion-user">
            <TextInput
              id="bastion-user"
              value={config.bastion.username}
              onChange={(_e, val) =>
                onChange({ bastion: { ...config.bastion, username: val } })
              }
              placeholder="core"
            />
          </FormGroup>
          <FormGroup label={t('Bastion password')} fieldId="bastion-password">
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
                    'Used to log in to the bastion desktop. Stored in a Secret and cleared from the cluster resource; leave blank to have one generated.',
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
