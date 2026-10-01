import React from 'react';
import { useTranslation } from 'react-i18next';
import { Alert } from '@patternfly/react-core';
import './CommunityDisclaimer.css';

const CommunityDisclaimer: React.FC = () => {
  const { t } = useTranslation('plugin__oct-shift-on-shift-virt');
  return (
    <Alert
      className="ct-community-disclaimer"
      variant="info"
      isInline
      title={t('Community project. Not officially supported by Red Hat.')}
    >
      {t('OpenShift Community Tools is unofficial UX enhancements for the OpenShift Console.')}
    </Alert>
  );
};

export default CommunityDisclaimer;
