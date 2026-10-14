import React, { FC, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  CodeBlock,
  CodeBlockAction,
  CodeBlockCode,
  ClipboardCopyButton,
  ExpandableSection,
  Tab,
  Tabs,
  TabTitleText,
} from '@patternfly/react-core';

const I18N = 'plugin__oct-shift-on-shift-virt';

interface ConfigViewerProps {
  installConfig?: string;
  agentConfig?: string;
}

const CopyableCodeBlock: FC<{ code: string; id: string }> = ({ code, id }) => {
  const { t } = useTranslation(I18N);
  const [copied, setCopied] = useState(false);

  const handleCopy = () => {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  const actions = (
    <CodeBlockAction>
      <ClipboardCopyButton
        id={`${id}-copy`}
        textId={`${id}-code`}
        aria-label={t('Copy to clipboard')}
        onClick={handleCopy}
        variant="plain"
      >
        {copied ? t('Copied!') : t('Copy')}
      </ClipboardCopyButton>
    </CodeBlockAction>
  );

  return (
    <CodeBlock actions={actions}>
      <CodeBlockCode id={`${id}-code`}>{code}</CodeBlockCode>
    </CodeBlock>
  );
};

const ConfigViewer: FC<ConfigViewerProps> = ({ installConfig, agentConfig }) => {
  const { t } = useTranslation(I18N);
  const [isExpanded, setIsExpanded] = useState(false);
  const [activeTab, setActiveTab] = useState<string | number>('install-config');

  if (!installConfig && !agentConfig) return null;

  return (
    <ExpandableSection
      toggleText={t('Generated Configurations')}
      isExpanded={isExpanded}
      onToggle={(_e, expanded) => setIsExpanded(expanded)}
    >
      <Tabs
        activeKey={activeTab}
        onSelect={(_e, key) => setActiveTab(key)}
        isFilled
      >
        {installConfig && (
          <Tab
            eventKey="install-config"
            title={<TabTitleText>{t('install-config.yaml')}</TabTitleText>}
          >
            <div style={{ paddingTop: '1rem' }}>
              <CopyableCodeBlock code={installConfig} id="install-config" />
            </div>
          </Tab>
        )}
        {agentConfig && (
          <Tab
            eventKey="agent-config"
            title={<TabTitleText>{t('agent-config.yaml')}</TabTitleText>}
          >
            <div style={{ paddingTop: '1rem' }}>
              <CopyableCodeBlock code={agentConfig} id="agent-config" />
            </div>
          </Tab>
        )}
      </Tabs>
    </ExpandableSection>
  );
};

export default ConfigViewer;
