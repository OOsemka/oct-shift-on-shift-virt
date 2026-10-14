import React, { FC, useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Form,
  FormGroup,
  Radio,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import { ClusterConfig, generateNodeNames } from './types';

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

const TopologyStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);
  const initializedRef = useRef(false);

  useEffect(() => {
    if (!initializedRef.current) {
      initializedRef.current = true;
      if (config.nodeNames.length === 0 && config.topology !== 'custom') {
        onChange({
          nodeNames: generateNodeNames(config.clusterName, config.topology, config.masters, config.workers),
        });
      }
    }
  });

  const handleSelect = (topo: (typeof TOPOLOGIES)[number]) => {
    const updatedMasters = { ...config.masters, count: topo.masters };
    const updatedWorkers = { ...config.workers, count: topo.workers };

    const patch: Partial<ClusterConfig> = {
      topology: topo.key,
      masters: updatedMasters,
      workers: updatedWorkers,
      infra: { ...config.infra, count: topo.infra },
    };

    if (topo.key !== 'custom') {
      patch.nodeNames = generateNodeNames(config.clusterName, topo.key, updatedMasters, updatedWorkers);
    }

    onChange(patch);
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
