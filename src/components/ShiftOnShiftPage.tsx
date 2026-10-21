import React, { FC, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ListPageHeader } from '@openshift-console/dynamic-plugin-sdk';
import {
  Breadcrumb,
  BreadcrumbItem,
  PageSection,
  Stack,
  StackItem,
  Tab,
  Tabs,
  TabTitleText,
} from '@patternfly/react-core';
import CommunityDisclaimer from './shared/CommunityDisclaimer';
import CreateClusterWizard from './InstallTab/CreateClusterWizard';
import ClusterTable from './ManageTab/ClusterTable';
import './ShiftOnShiftPage.css';

const I18N = 'plugin__oct-shift-on-shift-virt';

const ShiftOnShiftPage: FC = () => {
  const { t } = useTranslation(I18N);
  const [activeTab, setActiveTab] = useState<string | number>('install');

  const goComputeHub = () => {
    window.location.href = '/community-tools/compute';
  };

  return (
    <>
      <PageSection type="breadcrumb">
        <Breadcrumb>
          <BreadcrumbItem
            component="a"
            onClick={(e) => {
              e.preventDefault();
              goComputeHub();
            }}
          >
            {t('Compute')}
          </BreadcrumbItem>
          <BreadcrumbItem isActive>{t('Shift-on-Shift Virt')}</BreadcrumbItem>
        </Breadcrumb>
      </PageSection>
      <ListPageHeader title={t('Shift-on-Shift Virt')} />
      <PageSection>
        <Stack hasGutter>
          <StackItem>
            <CommunityDisclaimer />
          </StackItem>
          <StackItem>
            <Tabs
              activeKey={activeTab}
              onSelect={(_e, key) => setActiveTab(key)}
              className="sos-tabs"
            >
              <Tab
                eventKey="install"
                title={<TabTitleText>{t('Install')}</TabTitleText>}
              >
                <div className="sos-tab-content">
                  <CreateClusterWizard />
                </div>
              </Tab>
              <Tab
                eventKey="manage"
                title={<TabTitleText>{t('Manage')}</TabTitleText>}
              >
                <div className="sos-tab-content">
                  <ClusterTable />
                </div>
              </Tab>
            </Tabs>
          </StackItem>
        </Stack>
      </PageSection>
    </>
  );
};

export default ShiftOnShiftPage;
