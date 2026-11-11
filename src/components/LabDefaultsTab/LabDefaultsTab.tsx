import React, { FC, useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  k8sCreate,
  k8sGet,
  k8sPatch,
  K8sResourceCommon,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import {
  ActionGroup,
  Alert,
  Button,
  Checkbox,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Label,
  NumberInput,
  Spinner,
  TextArea,
  TextInput,
} from '@patternfly/react-core';
import { fetchOCPVersions } from '../../utils/api';
import { fetchIPPools, freeAddresses, IPPoolItem } from '../../utils/ippools';
import {
  DEFAULTS_NAMESPACE,
  DEFAULTS_SECRET,
  fetchErrorText,
  fetchLabDefaults,
  LabDefaults,
  LabSettings,
  pullSecretProblem,
  SecretModel,
  sshKeyProblem,
} from '../../utils/labdefaults';

const I18N = 'plugin__oct-shift-on-shift-virt';
type SC = K8sResourceCommon & { metadata: { name: string } };

const LabDefaultsTab: FC = () => {
  const { t } = useTranslation(I18N);
  const [defaults, setDefaults] = useState<LabDefaults | null>(null);
  const [loadError, setLoadError] = useState<string>();
  const [settings, setSettings] = useState<LabSettings>({});
  const [newPullSecret, setNewPullSecret] = useState('');
  const [sshKey, setSshKey] = useState('');
  const [versions, setVersions] = useState<string[]>(['5.0', '4.22']);
  const [latestReleases, setLatestReleases] = useState<Record<string, string>>({});
  const [pools, setPools] = useState<IPPoolItem[]>([]);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string>();
  const [saved, setSaved] = useState(false);
  const [scs] = useK8sWatchResource<SC[]>({
    groupVersionKind: { group: 'storage.k8s.io', version: 'v1', kind: 'StorageClass' },
    isList: true,
  });

  const load = useCallback(() => {
    fetchLabDefaults()
      .then((d) => {
        setDefaults(d);
        setSettings(d.settings || {});
        setSshKey(d.sshPublicKey || '');
        setLoadError(undefined);
      })
      .catch((e) => setLoadError(fetchErrorText(e)));
  }, []);

  useEffect(() => {
    load();
    fetchOCPVersions()
      .then((r) => {
        const { items, latest } = r as { items?: string[]; latest?: Record<string, string> };
        if (items?.length) setVersions(items);
        if (latest) setLatestReleases(latest);
      })
      .catch(() => undefined);
    fetchIPPools().then(setPools).catch(() => setPools([]));
  }, [load]);

  const update = (patch: Partial<LabSettings>) => {
    setSaved(false);
    setSettings((s) => ({ ...s, ...patch }));
  };
  const pullErr = newPullSecret.trim() ? pullSecretProblem(newPullSecret) : undefined;
  const sshErr = sshKey.trim() ? sshKeyProblem(sshKey) : undefined;
  const scNames = useMemo(() => (Array.isArray(scs) ? scs.map((s) => s.metadata.name).sort() : []), [scs]);

  const save = async () => {
    if (pullErr || sshErr) return;
    setSaving(true);
    setSaveError(undefined);
    setSaved(false);
    // Only what changed is written: the pull secret only when a new one was
    // pasted, so saving settings never needs (or exposes) the stored one.
    const stringData: Record<string, string> = { 'settings.json': JSON.stringify(settings) };
    if (newPullSecret.trim()) stringData.pullSecret = newPullSecret.trim();
    if (sshKey.trim() !== (defaults?.sshPublicKey || '')) stringData.sshPublicKey = sshKey.trim();
    try {
      let exists = true;
      try {
        await k8sGet({ model: SecretModel, name: DEFAULTS_SECRET, ns: DEFAULTS_NAMESPACE });
      } catch (e) {
        if ((e as { code?: number })?.code === 404 || /not found/i.test(fetchErrorText(e))) exists = false;
        else throw e;
      }
      if (exists) {
        await k8sPatch({
          model: SecretModel,
          resource: { metadata: { name: DEFAULTS_SECRET, namespace: DEFAULTS_NAMESPACE } } as K8sResourceCommon,
          data: [{ op: 'add', path: '/stringData', value: stringData }],
        });
      } else {
        await k8sCreate({
          model: SecretModel,
          data: {
            apiVersion: 'v1',
            kind: 'Secret',
            metadata: {
              name: DEFAULTS_SECRET,
              namespace: DEFAULTS_NAMESPACE,
              labels: { 'app.kubernetes.io/part-of': 'oct-shift-on-shift-virt' },
            },
            type: 'Opaque',
            stringData,
          } as K8sResourceCommon,
        });
      }
      setNewPullSecret('');
      setSaved(true);
      load();
    } catch (e) {
      const msg = fetchErrorText(e);
      setSaveError(
        /forbidden|403/i.test(msg)
          ? t('You do not have permission to change the lab defaults. Ask a cluster administrator. ({{msg}})', { msg })
          : msg,
      );
    } finally {
      setSaving(false);
    }
  };

  if (!defaults && !loadError) return <Spinner size="lg" />;

  return (
    <>
      <Alert variant="info" isInline isPlain title={t('One set of lab settings for every cluster')}>
        {t('Store the pull secret, your SSH public key and the usual settings once. The wizard can then leave the credentials empty, and the quick compact cluster on the Install tab needs only a name. The pull secret is stored in a Secret and never shown again; only cluster administrators can change these settings.')}
      </Alert>
      {loadError && (
        <Alert variant="danger" isInline title={t('Could not load the lab defaults')}>
          {loadError}
        </Alert>
      )}
      <Form className="sos-form">
        <FormGroup label={t('Pull secret')} fieldId="lab-pull">
          <div>
            {defaults?.pullSecretSet && !defaults.pullSecretProblem ? (
              <Label color="green" isCompact>{t('Set')}</Label>
            ) : defaults?.pullSecretProblem ? (
              <Label color="red" isCompact>{t('Invalid')}</Label>
            ) : (
              <Label color="orange" isCompact>{t('Not set')}</Label>
            )}
          </div>
          <TextArea
            id="lab-pull"
            value={newPullSecret}
            onChange={(_e, v) => {
              setSaved(false);
              setNewPullSecret(v);
            }}
            rows={3}
            placeholder={defaults?.pullSecretSet ? t('Paste a new pull secret to replace the stored one') : '{"auths": {...}}'}
            validated={pullErr ? 'error' : 'default'}
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant={pullErr || defaults?.pullSecretProblem ? 'error' : 'default'}>
                {pullErr || defaults?.pullSecretProblem || t('From console.redhat.com. Write-only: it is never displayed again.')}
              </HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FormGroup>
        <FormGroup label={t('SSH public key')} fieldId="lab-ssh">
          <TextArea
            id="lab-ssh"
            value={sshKey}
            onChange={(_e, v) => {
              setSaved(false);
              setSshKey(v);
            }}
            rows={2}
            placeholder="ssh-ed25519 AAAA... user@host"
            validated={sshErr ? 'error' : 'default'}
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant={sshErr ? 'error' : 'default'}>
                {sshErr ||
                  (defaults?.sshKeyFingerprint
                    ? t('Stored key: {{fp}}', { fp: defaults.sshKeyFingerprint })
                    : t('Authorized on every node and the bastion, so you can SSH in.'))}
              </HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FormGroup>
        <FormGroup label={t('Base domain')} fieldId="lab-domain">
          <TextInput id="lab-domain" value={settings.baseDomain || ''} onChange={(_e, v) => update({ baseDomain: v.trim() })} placeholder="example.com" />
        </FormGroup>
        <FormGroup label={t('OpenShift version')} fieldId="lab-version">
          <FormSelect id="lab-version" value={settings.ocpVersion || ''} onChange={(_e, v) => update({ ocpVersion: v })}>
            <FormSelectOption value="" label={t('Select a version')} isDisabled />
            {versions.map((v) => (
              <FormSelectOption
                key={v}
                value={v}
                label={latestReleases[v] ? t('{{version}} (currently {{release}})', { version: v, release: latestReleases[v] }) : v}
              />
            ))}
          </FormSelect>
        </FormGroup>
        <FormGroup label={t('Storage class for VM disks')} fieldId="lab-sc">
          <FormSelect id="lab-sc" value={settings.storageClassName || ''} onChange={(_e, v) => update({ storageClassName: v })}>
            <FormSelectOption value="" label={t('Cluster default')} />
            {scNames.map((n) => (
              <FormSelectOption key={n} value={n} label={n} />
            ))}
          </FormSelect>
        </FormGroup>
        <FormGroup label={t('IP pool for quick clusters')} fieldId="lab-pool">
          <FormSelect id="lab-pool" value={settings.ipPool || ''} onChange={(_e, v) => update({ ipPool: v })}>
            <FormSelectOption value="" label={t('Select an IP pool')} isDisabled />
            {pools.map((p) => (
              <FormSelectOption
                key={p.name}
                value={p.name}
                label={t('{{name}}: {{nad}}, {{free}} free', { name: p.name, nad: p.nadName, free: String(freeAddresses(p).length) })}
              />
            ))}
          </FormSelect>
          <FormHelperText>
            <HelperText>
              <HelperTextItem>
                {pools.length === 0 ? t('Create an IP pool in the IP Pools tab first.') : t('Quick clusters take the network, gateway and two free addresses from this pool.')}
              </HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FormGroup>
        <FormGroup label={t('Compact node size (each of 3 nodes)')} fieldId="lab-size">
          <div className="sos-lab-size">
            <span>{t('vCPU')}</span>
            <NumberInput id="lab-cores" value={settings.masterCores || 8} min={4} max={64}
              onMinus={() => update({ masterCores: Math.max(4, (settings.masterCores || 8) - 1) })}
              onPlus={() => update({ masterCores: Math.min(64, (settings.masterCores || 8) + 1) })}
              onChange={(e) => update({ masterCores: parseInt((e.target as HTMLInputElement).value, 10) || 8 })} widthChars={3} />
            <span>{t('Memory GiB')}</span>
            <NumberInput id="lab-mem" value={settings.masterMemoryGi || 32} min={16} max={512}
              onMinus={() => update({ masterMemoryGi: Math.max(16, (settings.masterMemoryGi || 32) - 8) })}
              onPlus={() => update({ masterMemoryGi: Math.min(512, (settings.masterMemoryGi || 32) + 8) })}
              onChange={(e) => update({ masterMemoryGi: parseInt((e.target as HTMLInputElement).value, 10) || 32 })} widthChars={4} />
            <span>{t('Disk GiB')}</span>
            <NumberInput id="lab-disk" value={settings.masterDiskGi || 120} min={120} max={2048}
              onMinus={() => update({ masterDiskGi: Math.max(120, (settings.masterDiskGi || 120) - 20) })}
              onPlus={() => update({ masterDiskGi: Math.min(2048, (settings.masterDiskGi || 120) + 20) })}
              onChange={(e) => update({ masterDiskGi: parseInt((e.target as HTMLInputElement).value, 10) || 120 })} widthChars={4} />
          </div>
        </FormGroup>
        <FormGroup label={t('Bastion desktop on quick clusters')} fieldId="lab-graphical">
          <Checkbox
            id="lab-graphical"
            isChecked={Boolean(settings.bastionGraphical)}
            onChange={(_e, v) => update({ bastionGraphical: v })}
            label={t('Install a desktop on the bastion by default (adds 2 vCPU / 4 GiB; can be changed per cluster)')}
          />
        </FormGroup>
        <FormGroup label={t('Host routes on quick clusters')} fieldId="lab-host-routes" isStack>
          <Checkbox
            id="lab-host-routes"
            isChecked={Boolean(settings.publishHostRoutes)}
            onChange={(_e, v) => update({ publishHostRoutes: v })}
            label={t('Publish the API and console through this cluster\'s routes by default (no DNS records needed; can be changed per cluster)')}
          />
          <Checkbox
            id="lab-publish-apps"
            isChecked={Boolean(settings.publishHostRoutes && settings.publishAppRoutes)}
            isDisabled={!settings.publishHostRoutes}
            onChange={(_e, v) => update({ publishAppRoutes: v })}
            label={t('Also publish all application routes of quick clusters (outside openshift-* and kube-*; can be changed per cluster)')}
          />
        </FormGroup>
        <FormGroup label={t('Nested storage and virtualization on quick clusters')} fieldId="lab-nested" isStack>
          <Checkbox
            id="lab-nested-storage"
            isChecked={Boolean(settings.nestedStorage || settings.nestedVirtualization)}
            isDisabled={Boolean(settings.nestedVirtualization)}
            onChange={(_e, v) => update({ nestedStorage: v })}
            label={t('Nested storage: expose a host storage class inside the cluster (KubeVirt CSI)')}
          />
          <Checkbox
            id="lab-nested-virt"
            isChecked={Boolean(settings.nestedVirtualization)}
            onChange={(_e, v) => update({ nestedVirtualization: v, ...(v ? { nestedStorage: true } : {}) })}
            label={t('Install OpenShift Virtualization in the cluster (needs nested storage)')}
          />
          {(settings.nestedStorage || settings.nestedVirtualization) && (
            <div className="sos-lab-size">
              <span>{t('Host storage class')}</span>
              <FormSelect
                id="lab-nested-sc"
                value={settings.nestedStorageClass || ''}
                onChange={(_e, v) => update({ nestedStorageClass: v })}
                aria-label={t('Host storage class')}
              >
                <FormSelectOption value="" label={t('Host default virtualization class')} />
                {scNames.map((n) => (
                  <FormSelectOption key={n} value={n} label={n} />
                ))}
              </FormSelect>
              <span>{t('Allowance GiB')}</span>
              <NumberInput
                id="lab-nested-gi"
                value={settings.nestedStorageAllowanceGi || 100}
                min={10}
                max={10000}
                onMinus={() => update({ nestedStorageAllowanceGi: Math.max(10, (settings.nestedStorageAllowanceGi || 100) - 10) })}
                onPlus={() => update({ nestedStorageAllowanceGi: Math.min(10000, (settings.nestedStorageAllowanceGi || 100) + 10) })}
                onChange={(e) => update({ nestedStorageAllowanceGi: parseInt((e.target as HTMLInputElement).value, 10) || 100 })}
                widthChars={5}
              />
            </div>
          )}
        </FormGroup>
        {saveError && (
          <Alert variant="danger" isInline title={t('Could not save the lab defaults')}>
            {saveError}
          </Alert>
        )}
        {saved && <Alert variant="success" isInline isPlain title={t('Lab defaults saved')} />}
        <ActionGroup>
          <Button variant="primary" onClick={save} isLoading={saving} isDisabled={saving || Boolean(pullErr || sshErr)}>
            {t('Save')}
          </Button>
        </ActionGroup>
      </Form>
    </>
  );
};

export default LabDefaultsTab;
