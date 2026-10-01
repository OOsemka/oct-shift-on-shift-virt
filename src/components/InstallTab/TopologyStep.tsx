import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Form,
  FormGroup,
  Radio,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import { ClusterConfig, NodeNameEntry } from './types';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
}

const TOPOLOGIES: { key: ClusterConfig['topology']; masters: number; workers: number; infra: number }[] = [
  { key: 'compact', masters: 3, workers: 0, infra: 0 },
  { key: 'ha', masters: 3, workers: 3, infra: 0 },
  { key: 'custom', masters: 3, workers: 0, infra: 0 },
];

const LABELS: Record<ClusterConfig['topology'], string> = {
  compact: '3-Node Compact',
  ha: 'Standard HA',
  custom: 'Custom',
};

const DESCRIPTIONS: Record<ClusterConfig['topology'], string> = {
  compact: '3 schedulable control-plane nodes, no dedicated workers. Smallest footprint.',
  ha: '3 control-plane nodes + 3 worker nodes. Standard production topology.',
  custom: 'Choose your own node counts and roles.',
};

function generateNames(prefix: string, count: number, role: NodeNameEntry['role']): NodeNameEntry[] {
  return Array.from({ length: count }, (_, i) => ({
    name: `${prefix}-${role}-${i}`,
    role,
  }));
}

const TopologyStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);

  const handleSelect = (topo: (typeof TOPOLOGIES)[number]) => {
    const prefix = config.clusterName || 'cluster';
    const names = [
      ...generateNames(prefix, topo.masters, 'master'),
      ...generateNames(prefix, topo.workers, 'worker'),
      ...generateNames(prefix, topo.infra, 'infra'),
    ];
    onChange({
      topology: topo.key,
      masters: { ...config.masters, count: topo.masters },
      workers: { ...config.workers, count: topo.workers },
      infra: { ...config.infra, count: topo.infra },
      nodeNames: names,
    });
  };

  return (
    <Form className="sos-form">
      <FormGroup label={t('Cluster Topology')} fieldId="topology" role="radiogroup">
        <Stack hasGutter>
          {TOPOLOGIES.map((topo) => (
            <StackItem key={topo.key}>
              <Radio
                id={`topology-${topo.key}`}
                name="topology-selection"
                label={t(LABELS[topo.key])}
                description={t(DESCRIPTIONS[topo.key])}
                isChecked={config.topology === topo.key}
                onChange={() => handleSelect(topo)}
              />
            </StackItem>
          ))}
        </Stack>
      </FormGroup>
    </Form>
  );
};

export default TopologyStep;
