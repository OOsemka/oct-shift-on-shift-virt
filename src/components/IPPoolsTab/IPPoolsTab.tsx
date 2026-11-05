import React, { FC, useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  k8sCreate,
  k8sDelete,
  K8sResourceCommon,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Button,
  EmptyState,
  EmptyStateActions,
  EmptyStateBody,
  EmptyStateFooter,
  Form,
  FormGroup,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Label,
  LabelGroup,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  NumberInput,
  Spinner,
  TextArea,
  TextInput,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { NetworkIcon } from '@patternfly/react-icons';
import { ExpandableRowContent, Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import {
  buildIPPool,
  expandAddresses,
  addressEntries,
  fetchIPPools,
  IPPoolItem,
  IPPoolModel,
  PoolForm,
  POOL_NAMESPACE,
  poolProblems,
} from '../../utils/ippools';

const I18N = 'plugin__oct-shift-on-shift-virt';

type NAD = K8sResourceCommon & { metadata: { name: string; namespace: string } };

const EMPTY_FORM: PoolForm = { name: '', nadName: '', prefixLength: 24, addresses: '', gateway: '', description: '' };

function errorText(e: unknown): string {
  const body = (e as { json?: { message?: string; error?: string }; code?: number })?.json;
  const code = (e as { code?: number; response?: { status?: number } })?.code ?? (e as { response?: { status?: number } })?.response?.status;
  const msg = body?.message || body?.error || (e instanceof Error ? e.message : String(e));
  return code === 403 || /forbidden/i.test(msg) ? `403: ${msg}` : msg;
}

const IPPoolsTab: FC = () => {
  const { t } = useTranslation(I18N);
  const [pools, setPools] = useState<IPPoolItem[] | null>(null);
  const [loadError, setLoadError] = useState<string>();
  const [expanded, setExpanded] = useState<string[]>([]);
  const [creating, setCreating] = useState(false);
  const [form, setForm] = useState<PoolForm>(EMPTY_FORM);
  const [touched, setTouched] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string>();
  const [deleteTarget, setDeleteTarget] = useState<IPPoolItem | null>(null);
  const [deleteError, setDeleteError] = useState<string>();

  const [nads, nadsLoaded, nadsError] = useK8sWatchResource<NAD[]>({
    groupVersionKind: { group: 'k8s.cni.cncf.io', version: 'v1', kind: 'NetworkAttachmentDefinition' },
    isList: true,
  });
  const nadOptions = useMemo(
    () =>
      (Array.isArray(nads) ? nads : [])
        .map((n) => `${n.metadata.namespace}/${n.metadata.name}`)
        .sort(),
    [nads],
  );

  const load = useCallback(() => {
    fetchIPPools()
      .then((p) => {
        setPools(p);
        setLoadError(undefined);
      })
      .catch((e) => setLoadError(errorText(e)));
  }, []);

  useEffect(() => {
    load();
    const id = setInterval(load, 30000);
    return () => clearInterval(id);
  }, [load]);

  const problems = poolProblems(form, (pools || []).map((p) => p.name));
  const problemFor = (field: keyof PoolForm) => {
    const p = problems.find((x) => x.field === field);
    return p && touched ? t(p.key, p.values) : undefined;
  };
  const preview = expandAddresses(addressEntries(form.addresses));
  const update = (patch: Partial<PoolForm>) => {
    setTouched(true);
    setForm((f) => ({ ...f, ...patch }));
  };

  const closeCreate = () => {
    setCreating(false);
    setForm(EMPTY_FORM);
    setTouched(false);
    setSaveError(undefined);
  };

  const save = async () => {
    setTouched(true);
    if (problems.length > 0) return;
    setSaving(true);
    setSaveError(undefined);
    try {
      await k8sCreate({ model: IPPoolModel, data: buildIPPool(form) as K8sResourceCommon, ns: POOL_NAMESPACE });
      closeCreate();
      load();
    } catch (e) {
      const msg = errorText(e);
      setSaveError(
        msg.startsWith('403')
          ? t('You do not have permission to create IP pools. Ask a cluster administrator. ({{msg}})', { msg })
          : msg,
      );
    } finally {
      setSaving(false);
    }
  };

  const remove = async () => {
    if (!deleteTarget) return;
    setDeleteError(undefined);
    try {
      await k8sDelete({
        model: IPPoolModel,
        resource: { metadata: { name: deleteTarget.name, namespace: POOL_NAMESPACE } } as K8sResourceCommon,
      });
      setDeleteTarget(null);
      load();
    } catch (e) {
      const msg = errorText(e);
      setDeleteError(
        msg.startsWith('403') ? t('You do not have permission to delete IP pools. Ask a cluster administrator. ({{msg}})', { msg }) : msg,
      );
    }
  };

  const inUseBy = (p: IPPoolItem) =>
    Array.from(new Set(p.addresses.filter((a) => a.usedBy).map((a) => a.usedBy!.cluster))).sort();

  return (
    <>
      <Alert variant="info" isInline isPlain title={t('Prepare external addresses once, pick them from a list later')}>
        {t('An IP pool is a set of free addresses on one external VLAN (network attachment). When creating a cluster, choose a pool on the External Access step and the free addresses are offered there. An address is in use while a cluster references it; deleting the cluster frees it.')}
      </Alert>

      <Toolbar>
        <ToolbarContent>
          <ToolbarItem>
            <Button variant="primary" onClick={() => setCreating(true)}>
              {t('Create IP pool')}
            </Button>
          </ToolbarItem>
          <ToolbarItem>
            <Button variant="secondary" onClick={load}>
              {t('Refresh')}
            </Button>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {loadError && (
        <Alert variant="danger" isInline title={t('Could not load IP pools')}>
          {loadError}
        </Alert>
      )}

      {pools === null && !loadError ? (
        <Spinner size="lg" />
      ) : pools && pools.length === 0 ? (
        <EmptyState titleText={t('No IP pools yet')} icon={NetworkIcon} headingLevel="h3">
          <EmptyStateBody>
            {t('Create a pool with the free addresses of an external VLAN, so people creating clusters do not have to guess which addresses are available.')}
          </EmptyStateBody>
          <EmptyStateFooter>
            <EmptyStateActions>
              <Button variant="primary" onClick={() => setCreating(true)}>
                {t('Create IP pool')}
              </Button>
            </EmptyStateActions>
          </EmptyStateFooter>
        </EmptyState>
      ) : (
        <Table aria-label={t('IP pools')} variant="compact">
          <Thead>
            <Tr>
              <Th screenReaderText={t('Details')} />
              <Th>{t('Name')}</Th>
              <Th>{t('Network (NAD)')}</Th>
              <Th>{t('Prefix')}</Th>
              <Th>{t('Gateway')}</Th>
              <Th>{t('Free')}</Th>
              <Th>{t('Description')}</Th>
              <Th screenReaderText={t('Actions')} />
            </Tr>
          </Thead>
          {(pools || []).map((p, i) => {
            const isOpen = expanded.includes(p.name);
            return (
              <Tbody key={p.name} isExpanded={isOpen}>
                <Tr>
                  <Td
                    expand={{
                      rowIndex: i,
                      isExpanded: isOpen,
                      onToggle: () =>
                        setExpanded((e) => (isOpen ? e.filter((n) => n !== p.name) : [...e, p.name])),
                    }}
                  />
                  <Td dataLabel={t('Name')}>{p.name}</Td>
                  <Td dataLabel={t('Network (NAD)')}>{p.nadName}</Td>
                  <Td dataLabel={t('Prefix')}>/{p.prefixLength}</Td>
                  <Td dataLabel={t('Gateway')}>{p.gateway || '—'}</Td>
                  <Td dataLabel={t('Free')}>
                    {p.problem ? (
                      <Label color="red" isCompact>
                        {t('Invalid')}
                      </Label>
                    ) : (
                      <Label color={p.free === 0 ? 'orange' : 'green'} isCompact>
                        {t('{{free}} of {{total}}', { free: String(p.free), total: String(p.total) })}
                      </Label>
                    )}
                  </Td>
                  <Td dataLabel={t('Description')}>{p.description || '—'}</Td>
                  <Td isActionCell>
                    <Button variant="link" isDanger onClick={() => setDeleteTarget(p)}>
                      {t('Delete')}
                    </Button>
                  </Td>
                </Tr>
                <Tr isExpanded={isOpen}>
                  <Td colSpan={8}>
                    <ExpandableRowContent>
                      {p.problem ? (
                        <Alert variant="danger" isInline isPlain title={t('This pool offers no addresses until it is fixed')}>
                          {p.problem}
                        </Alert>
                      ) : (
                        <LabelGroup numLabels={64} isCompact>
                          {p.addresses.map((a) => (
                            <Label key={a.ip} isCompact color={a.usedBy ? 'blue' : 'green'}>
                              {a.usedBy
                                ? t('{{ip}}: {{cluster}} ({{role}})', { ip: a.ip, cluster: a.usedBy.cluster, role: a.usedBy.role })
                                : t('{{ip}}: free', { ip: a.ip })}
                            </Label>
                          ))}
                        </LabelGroup>
                      )}
                    </ExpandableRowContent>
                  </Td>
                </Tr>
              </Tbody>
            );
          })}
        </Table>
      )}

      <Modal isOpen={creating} onClose={closeCreate} variant="medium" aria-labelledby="sos-pool-create-title">
        <ModalHeader labelId="sos-pool-create-title" title={t('Create IP pool')} />
        <ModalBody>
          <Form>
            <FormGroup label={t('Name')} isRequired fieldId="pool-name">
              <TextInput id="pool-name" value={form.name} onChange={(_e, v) => update({ name: v })} validated={problemFor('name') ? 'error' : 'default'} />
              {problemFor('name') && (
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem variant="error">{problemFor('name')}</HelperTextItem>
                  </HelperText>
                </FormHelperText>
              )}
            </FormGroup>
            <FormGroup label={t('External network (NAD)')} isRequired fieldId="pool-nad">
              {!nadsLoaded && !nadsError ? (
                <Spinner size="md" />
              ) : nadsError ? (
                <TextInput
                  id="pool-nad"
                  value={form.nadName}
                  placeholder="namespace/name"
                  onChange={(_e, v) => update({ nadName: v.trim() })}
                />
              ) : (
                <FormSelect id="pool-nad" value={form.nadName} onChange={(_e, v) => update({ nadName: v })}>
                  <FormSelectOption value="" label={t('Select a NetworkAttachmentDefinition')} isDisabled />
                  {nadOptions.map((n) => (
                    <FormSelectOption key={n} value={n} label={n} />
                  ))}
                </FormSelect>
              )}
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant={problemFor('nadName') ? 'error' : 'default'}>
                    {problemFor('nadName') || t('The VLAN network the addresses belong to, the same one picked on the External Access step.')}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FormGroup>
            <FormGroup label={t('VLAN prefix length')} isRequired fieldId="pool-prefix">
              <NumberInput
                id="pool-prefix"
                value={form.prefixLength}
                min={8}
                max={30}
                onMinus={() => update({ prefixLength: Math.max(8, form.prefixLength - 1) })}
                onPlus={() => update({ prefixLength: Math.min(30, form.prefixLength + 1) })}
                onChange={(e) => {
                  const n = parseInt((e.target as HTMLInputElement).value, 10);
                  update({ prefixLength: isNaN(n) ? 0 : n });
                }}
                widthChars={4}
              />
              {problemFor('prefixLength') && (
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem variant="error">{problemFor('prefixLength')}</HelperTextItem>
                  </HelperText>
                </FormHelperText>
              )}
            </FormGroup>
            <FormGroup label={t('Free addresses')} isRequired fieldId="pool-addresses">
              <TextArea
                id="pool-addresses"
                value={form.addresses}
                onChange={(_e, v) => update({ addresses: v })}
                rows={4}
                placeholder={'192.0.2.10-192.0.2.40\n192.0.2.50'}
                validated={problemFor('addresses') ? 'error' : 'default'}
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant={problemFor('addresses') ? 'error' : 'default'}>
                    {problemFor('addresses') ||
                      t('One address or range per line. Only list addresses nothing else uses on that VLAN. Addresses: {{n}}.', {
                        n: String(preview.addresses.length),
                      })}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FormGroup>
            <FormGroup label={t('Gateway (optional)')} fieldId="pool-gateway">
              <TextInput id="pool-gateway" value={form.gateway} onChange={(_e, v) => update({ gateway: v })} validated={problemFor('gateway') ? 'error' : 'default'} />
              {problemFor('gateway') && (
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem variant="error">{problemFor('gateway')}</HelperTextItem>
                  </HelperText>
                </FormHelperText>
              )}
            </FormGroup>
            <FormGroup label={t('Description (optional)')} fieldId="pool-description">
              <TextInput id="pool-description" value={form.description} onChange={(_e, v) => update({ description: v })} />
            </FormGroup>
          </Form>
          {saveError && (
            <Alert variant="danger" isInline title={t('Could not create the pool')}>
              {saveError}
            </Alert>
          )}
        </ModalBody>
        <ModalFooter>
          <Button variant="primary" onClick={save} isLoading={saving} isDisabled={saving || (touched && problems.length > 0)}>
            {t('Create')}
          </Button>
          <Button variant="link" onClick={closeCreate}>
            {t('Cancel')}
          </Button>
        </ModalFooter>
      </Modal>

      <Modal isOpen={Boolean(deleteTarget)} onClose={() => setDeleteTarget(null)} variant="small" aria-labelledby="sos-pool-delete-title">
        <ModalHeader labelId="sos-pool-delete-title" title={t('Delete IP pool {{name}}?', { name: deleteTarget?.name || '' })} titleIconVariant="warning" />
        <ModalBody>
          {deleteTarget && inUseBy(deleteTarget).length > 0 ? (
            <Alert variant="warning" isInline isPlain title={t('Addresses from this pool are in use')}>
              {t('Clusters {{clusters}} keep working and keep their addresses; new clusters can no longer pick from this pool.', {
                clusters: inUseBy(deleteTarget).join(', '),
              })}
            </Alert>
          ) : (
            t('No cluster uses addresses from this pool.')
          )}
          {deleteError && (
            <Alert variant="danger" isInline title={t('Could not delete the pool')}>
              {deleteError}
            </Alert>
          )}
        </ModalBody>
        <ModalFooter>
          <Button variant="danger" onClick={remove}>
            {t('Delete')}
          </Button>
          <Button variant="link" onClick={() => setDeleteTarget(null)}>
            {t('Cancel')}
          </Button>
        </ModalFooter>
      </Modal>
    </>
  );
};

export default IPPoolsTab;
