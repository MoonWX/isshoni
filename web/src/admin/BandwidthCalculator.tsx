import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { Notice } from '../auth/Notice';
import { errorMessage } from '../lib/errorText';
import {
  BandwidthPresetAuto,
  BandwidthPresetMovie,
  BandwidthQuality1080p60,
  BandwidthQuality1440p60,
  BandwidthQuality2160p60,
  CodeBadRequest,
  ParamField,
  type BandwidthEstimate,
  type BandwidthInput,
  type BandwidthPreset,
  type BandwidthQuality,
} from '../protocol/api.gen';
import { isApiError } from '../protocol/rest';
import { Button } from '../ui/Button';
import { Field, inputClass, TextField } from '../ui/Field';
import { bandwidthQueryOptions } from './adminApi';
import { Section } from './AdminPage';
import styles from './BandwidthCalculator.module.css';
import { Format } from './format';

/** The M1 exit scenario of 04 §13.4 (doctor.DefaultBandwidthInput): what the form starts with. */
export const DEFAULT_BANDWIDTH_INPUT: BandwidthInput = {
  people: 5,
  sharing: 2,
  thumbnails: 8,
  quality: BandwidthQuality1080p60,
  preset: BandwidthPresetAuto,
  hours: 2,
};

const QUALITIES: readonly BandwidthQuality[] = [
  BandwidthQuality1080p60,
  BandwidthQuality1440p60,
  BandwidthQuality2160p60,
];
const PRESETS: readonly BandwidthPreset[] = [BandwidthPresetAuto, BandwidthPresetMovie];

/** The ranges the server takes (doctor.BandwidthInputProblem). */
const MAX_PEOPLE = 1000;
const MAX_THUMBNAILS = 100;
const MAX_HOURS = 8784;

type NumberField = 'people' | 'sharing' | 'thumbnails' | 'hours';

/** What the admin typed, as typed. */
interface Draft {
  people: string;
  sharing: string;
  thumbnails: string;
  quality: BandwidthQuality;
  preset: BandwidthPreset;
  hours: string;
}

function draftOf(input: BandwidthInput): Draft {
  return {
    people: String(input.people),
    sharing: String(input.sharing),
    thumbnails: String(input.thumbnails),
    quality: input.quality,
    preset: input.preset,
    hours: String(input.hours),
  };
}

/** A whole number from min to max, or null. */
function wholeNumber(text: string, min: number, max: number): number | null {
  if (!/^\d+$/.test(text.trim())) return null;
  const n = Number(text.trim());
  return n >= min && n <= max ? n : null;
}

/** The input of a draft, or the fields that are out of range. */
export function checkDraft(d: Draft): { input: BandwidthInput } | { invalid: NumberField[] } {
  const people = wholeNumber(d.people, 1, MAX_PEOPLE);
  const sharing = wholeNumber(d.sharing, 0, people ?? MAX_PEOPLE);
  const thumbnails = wholeNumber(d.thumbnails, 0, MAX_THUMBNAILS);
  const hoursText = d.hours.trim().replace(',', '.');
  const hours = /^\d+(\.\d+)?$/.test(hoursText) ? Number(hoursText) : NaN;
  const invalid: NumberField[] = [];
  if (people === null) invalid.push('people');
  if (sharing === null) invalid.push('sharing');
  if (thumbnails === null) invalid.push('thumbnails');
  if (!(hours > 0 && hours <= MAX_HOURS)) invalid.push('hours');
  if (invalid.length > 0 || people === null || sharing === null || thumbnails === null) return { invalid };
  return { input: { people, sharing, thumbnails, quality: d.quality, preset: d.preset, hours } };
}

/**
 * The bandwidth calculator (05 §15.3, 04 §13.4): people, sharing, thumbnails, quality, preset and hours, answered
 * by GET /api/v1/admin/bandwidth, the same function as `isshoni doctor --people …`. It shows the egress, the rate
 * to each person and the gigabytes per session.
 */
export function BandwidthCalculator() {
  const { t, i18n } = useTranslation();
  const [draft, setDraft] = useState<Draft>(() => draftOf(DEFAULT_BANDWIDTH_INPUT));
  const [input, setInput] = useState<BandwidthInput>(DEFAULT_BANDWIDTH_INPUT);
  const [invalid, setInvalid] = useState<readonly NumberField[]>([]);
  const estimate = useQuery({ ...bandwidthQueryOptions(input), placeholderData: keepPreviousData });

  // The server names a field it refuses (400 bad_request, params.field).
  const refused =
    isApiError(estimate.error, CodeBadRequest) && typeof estimate.error.params?.[ParamField] === 'string'
      ? estimate.error.params[ParamField]
      : null;
  const errorOf = (field: NumberField): string | undefined => {
    if (!invalid.includes(field) && refused !== field) return undefined;
    switch (field) {
      case 'people':
        return t('admin.doctor.bandwidth.range', { min: 1, max: MAX_PEOPLE });
      case 'sharing':
        return t('admin.doctor.bandwidth.sharingRange');
      case 'thumbnails':
        return t('admin.doctor.bandwidth.range', { min: 0, max: MAX_THUMBNAILS });
      case 'hours':
        return t('admin.doctor.bandwidth.hoursRange', { max: MAX_HOURS });
    }
  };

  const number = (field: NumberField, label: string, decimal = false) => (
    <TextField
      label={label}
      name={field}
      inputMode={decimal ? 'decimal' : 'numeric'}
      autoComplete="off"
      value={draft[field]}
      onChange={(e) => {
        setDraft({ ...draft, [field]: e.target.value });
      }}
      error={errorOf(field)}
    />
  );

  return (
    <Section title={t('admin.doctor.bandwidth.title')} lead={t('admin.doctor.bandwidth.lead')}>
      <form
        className={styles.form}
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          const checked = checkDraft(draft);
          if ('invalid' in checked) {
            setInvalid(checked.invalid);
            const first = checked.invalid[0];
            if (first !== undefined) e.currentTarget.querySelector<HTMLElement>(`[name="${first}"]`)?.focus();
            return;
          }
          setInvalid([]);
          setInput(checked.input);
        }}
      >
        {number('people', t('admin.doctor.bandwidth.people'))}
        {number('sharing', t('admin.doctor.bandwidth.sharing'))}
        {number('thumbnails', t('admin.doctor.bandwidth.thumbnails'))}
        {number('hours', t('admin.doctor.bandwidth.hours'), true)}
        <Field label={t('admin.doctor.bandwidth.quality')}>
          {(control) => (
            <select
              {...control}
              className={inputClass}
              name="quality"
              value={draft.quality}
              onChange={(e) => {
                setDraft({ ...draft, quality: e.target.value as BandwidthQuality });
              }}
            >
              {QUALITIES.map((q) => (
                <option key={q} value={q}>
                  {t(`admin.doctor.bandwidth.qualities.${q}`)}
                </option>
              ))}
            </select>
          )}
        </Field>
        <Field label={t('admin.doctor.bandwidth.preset')}>
          {(control) => (
            <select
              {...control}
              className={inputClass}
              name="preset"
              value={draft.preset}
              onChange={(e) => {
                setDraft({ ...draft, preset: e.target.value as BandwidthPreset });
              }}
            >
              {PRESETS.map((p) => (
                <option key={p} value={p}>
                  {t(`admin.doctor.bandwidth.presets.${p}`)}
                </option>
              ))}
            </select>
          )}
        </Field>
        <div className={styles.wide}>
          <Button type="submit" loading={estimate.isFetching}>
            {t('admin.doctor.bandwidth.submit')}
          </Button>
        </div>
      </form>
      {estimate.isError && refused === null && (
        <Notice>{t('admin.loadFailed', { reason: errorMessage(estimate.error, t) })}</Notice>
      )}
      {estimate.data && <Estimate estimate={estimate.data} lang={i18n.language} />}
    </Section>
  );
}

function Estimate({ estimate, lang }: { estimate: BandwidthEstimate; lang: string }) {
  const { t } = useTranslation();
  const f = new Format(lang);
  const mbps = (n: number) => t('admin.doctor.bandwidth.mbps', { value: f.number(n, 2) });
  const { input } = estimate;
  return (
    <div className={styles.result} role="status">
      <p className={styles.headline}>
        {t('admin.doctor.bandwidth.scenario', {
          people: input.people,
          sharing: input.sharing,
          hours: f.number(input.hours, Number.isInteger(input.hours) ? 0 : 1),
        })}
      </p>
      <dl className={styles.figures}>
        <div>
          <dt>{t('admin.doctor.bandwidth.egress')}</dt>
          <dd>
            {mbps(estimate.egressMediaMbps)}
            <span className={styles.detail}>
              {t('admin.doctor.bandwidth.wire', { value: mbps(estimate.egressWireMbps) })}
            </span>
          </dd>
        </div>
        <div>
          <dt>{t('admin.doctor.bandwidth.session')}</dt>
          <dd>{t('admin.doctor.bandwidth.gb', { value: f.number(estimate.transferPerSessionGb, 1) })}</dd>
        </div>
        <div>
          <dt>{t('admin.doctor.bandwidth.perViewer')}</dt>
          <dd>
            {mbps(estimate.perViewerMbps.viewer)}
            <span className={styles.detail}>
              {t('admin.doctor.bandwidth.perSharer', { value: mbps(estimate.perViewerMbps.sharer) })}
            </span>
          </dd>
        </div>
        <div>
          <dt>{t('admin.doctor.bandwidth.ingress')}</dt>
          <dd>{mbps(estimate.ingressMediaMbps)}</dd>
        </div>
      </dl>
    </div>
  );
}
