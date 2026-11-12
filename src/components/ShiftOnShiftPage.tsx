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
import IPPoolsTab from './IPPoolsTab/IPPoolsTab';
import LabDefaultsTab from './LabDefaultsTab/LabDefaultsTab';
import MastersTab from './MastersTab/MastersTab';
import QuickClusterCard from './InstallTab/QuickClusterCard';
import './ShiftOnShiftPage.css';

const I18N = 'plugin__oct-shift-on-shift-virt';

const ShiftOnShiftPage: FC = () => {
  const { t } = useTranslation(I18N);
  const [activeTab, setActiveTab] = useState<string | number>('install');
  // Every tab stays mounted, so the Install tab would keep what it loaded when
  // the page opened (lab defaults, masters, quick plan). Bump this whenever the
  // Install tab is shown again so it reloads them, e.g. after saving the lab
  // defaults or making a master.
  const [installRefresh, setInstallRefresh] = useState(0);
  const selectTab = (key: string | number) => {
    if (key === 'install' && activeTab !== 'install') setInstallRefresh((n) => n + 1);
    setActiveTab(key);
  };

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
              onSelect={(_e, key) => selectTab(key)}
              className="sos-tabs"
            >
              <Tab
                eventKey="install"
                title={<TabTitleText>{t('Install')}</TabTitleText>}
              >
                <div className="sos-tab-content">
                  <QuickClusterCard refreshKey={installRefresh} onOpenDefaults={() => selectTab('defaults')} />
                  <CreateClusterWizard refreshKey={installRefresh} />
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
              <Tab
                eventKey="masters"
                title={<TabTitleText>{t('Masters')}</TabTitleText>}
              >
                <div className="sos-tab-content">
                  <MastersTab />
                </div>
              </Tab>
              <Tab
                eventKey="ippools"
                title={<TabTitleText>{t('IP Pools')}</TabTitleText>}
              >
                <div className="sos-tab-content">
                  <IPPoolsTab />
                </div>
              </Tab>
              <Tab
                eventKey="defaults"
                title={<TabTitleText>{t('Lab Defaults')}</TabTitleText>}
              >
                <div className="sos-tab-content">
                  <LabDefaultsTab />
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
