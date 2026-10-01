import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Button,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  TextInput,
} from '@patternfly/react-core';
import { MinusCircleIcon, PlusCircleIcon } from '@patternfly/react-icons';
import { ClusterConfig, NodeNameEntry } from './types';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
}

const NodeNamingStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);

  const updateNode = (idx: number, patch: Partial<NodeNameEntry>) => {
    const names = [...config.nodeNames];
    names[idx] = { ...names[idx], ...patch };
    onChange({ nodeNames: names });
  };

  const addNode = () => {
    const prefix = config.clusterName || 'node';
    onChange({
      nodeNames: [
        ...config.nodeNames,
        { name: `${prefix}-worker-${config.nodeNames.length}`, role: 'worker' },
      ],
    });
  };

  const removeNode = (idx: number) => {
    onChange({ nodeNames: config.nodeNames.filter((_, i) => i !== idx) });
  };

  return (
    <Form className="sos-form">
      {config.nodeNames.map((node, idx) => (
        <div key={idx} style={{ display: 'flex', gap: '8px', alignItems: 'flex-end', marginBottom: '8px' }}>
          <FormGroup label={idx === 0 ? t('Node Name') : undefined} fieldId={`node-name-${idx}`} style={{ flex: 2 }}>
            <TextInput
              id={`node-name-${idx}`}
              value={node.name}
              onChange={(_e, val) => updateNode(idx, { name: val })}
            />
          </FormGroup>
          <FormGroup label={idx === 0 ? t('Role') : undefined} fieldId={`node-role-${idx}`} style={{ flex: 1 }}>
            <FormSelect
              id={`node-role-${idx}`}
              value={node.role}
              onChange={(_e, val) => updateNode(idx, { role: val as NodeNameEntry['role'] })}
            >
              <FormSelectOption value="master" label={t('Master')} />
              <FormSelectOption value="worker" label={t('Worker')} />
              <FormSelectOption value="infra" label={t('Infra')} />
            </FormSelect>
          </FormGroup>
          <Button
            variant="plain"
            aria-label="Remove node"
            onClick={() => removeNode(idx)}
            isDisabled={node.role === 'master' && config.nodeNames.filter((n) => n.role === 'master').length <= 3}
          >
            <MinusCircleIcon />
          </Button>
        </div>
      ))}
      <Button variant="link" icon={<PlusCircleIcon />} onClick={addNode}>
        {t('Add node')}
      </Button>
    </Form>
  );
};

export default NodeNamingStep;
