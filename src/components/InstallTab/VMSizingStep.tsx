import React, { FC } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Form,
  FormGroup,
  FormSection,
  NumberInput,
} from '@patternfly/react-core';
import { ClusterConfig, NodeSizing } from './types';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface Props {
  config: ClusterConfig;
  onChange: (patch: Partial<ClusterConfig>) => void;
}

const ROLES = ['masters', 'workers', 'infra'] as const;
const ROLE_LABELS: Record<(typeof ROLES)[number], string> = {
  masters: 'Master',
  workers: 'Worker',
  infra: 'Infra',
};

const VMSizingStep: FC<Props> = ({ config, onChange }) => {
  const { t } = useTranslation(I18N);

  const updateSizing = (role: (typeof ROLES)[number], field: keyof NodeSizing, value: number) => {
    onChange({ [role]: { ...config[role], [field]: Math.max(1, value) } });
  };

  return (
    <Form className="sos-form">
      {ROLES.map((role) => {
        if (config[role].count === 0) return null;
        const sizing = config[role];
        return (
          <FormSection key={role} title={`${t(ROLE_LABELS[role])} (${sizing.count} nodes)`}>
            <FormGroup label={t('CPU Cores')} fieldId={`${role}-cpu`}>
              <NumberInput
                id={`${role}-cpu`}
                value={sizing.cpuCores}
                min={1}
                max={128}
                onMinus={() => updateSizing(role, 'cpuCores', sizing.cpuCores - 1)}
                onPlus={() => updateSizing(role, 'cpuCores', sizing.cpuCores + 1)}
                onChange={(e) => updateSizing(role, 'cpuCores', Number((e.target as HTMLInputElement).value))}
              />
            </FormGroup>
            <FormGroup label={t('Memory (Gi)')} fieldId={`${role}-mem`}>
              <NumberInput
                id={`${role}-mem`}
                value={sizing.memoryGi}
                min={4}
                max={1024}
                onMinus={() => updateSizing(role, 'memoryGi', sizing.memoryGi - 4)}
                onPlus={() => updateSizing(role, 'memoryGi', sizing.memoryGi + 4)}
                onChange={(e) => updateSizing(role, 'memoryGi', Number((e.target as HTMLInputElement).value))}
              />
            </FormGroup>
            <FormGroup label={t('Disk (Gi)')} fieldId={`${role}-disk`}>
              <NumberInput
                id={`${role}-disk`}
                value={sizing.diskGi}
                min={20}
                max={2048}
                onMinus={() => updateSizing(role, 'diskGi', sizing.diskGi - 20)}
                onPlus={() => updateSizing(role, 'diskGi', sizing.diskGi + 20)}
                onChange={(e) => updateSizing(role, 'diskGi', Number((e.target as HTMLInputElement).value))}
              />
            </FormGroup>
            {role !== 'masters' && (
              <FormGroup label={t('Count')} fieldId={`${role}-count`}>
                <NumberInput
                  id={`${role}-count`}
                  value={sizing.count}
                  min={0}
                  max={20}
                  onMinus={() => updateSizing(role, 'count', sizing.count - 1)}
                  onPlus={() => updateSizing(role, 'count', sizing.count + 1)}
                  onChange={(e) => updateSizing(role, 'count', Number((e.target as HTMLInputElement).value))}
                />
              </FormGroup>
            )}
          </FormSection>
        );
      })}
    </Form>
  );
};

export default VMSizingStep;
