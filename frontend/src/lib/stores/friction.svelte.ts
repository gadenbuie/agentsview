import { m } from "../i18n/index.js";
import { FrictionService } from "../api/generated/index";
import type { FrictionDigestItem, FrictionDigestResponse } from "../api/generated/index.js";
import { isAbortError, isNotFoundError } from "../api/runtime.js";
import { LatestRead } from "../utils/latest-read.js";
import { perf } from "./perf.svelte.js";

type ReadStatus = "ok" | "error" | "aborted";

function errorMessage(e: unknown): string {
  return e instanceof Error && e.message ? e.message : m.friction_load_error();
}

function newestFirst(a: FrictionDigestItem, b: FrictionDigestItem): number {
  if (a.date < b.date) return 1;
  if (a.date > b.date) return -1;
  return 0;
}

class FrictionStore {
  dates: FrictionDigestItem[] = $state([]);
  selectedDate: string | null = $state(null);
  digest: FrictionDigestResponse | null = $state(null);
  markdown: string | null = $state(null);
  loading = $state({ dates: false, digest: false, markdown: false });
  errors = $state<{
    dates: string | null;
    digest: string | null;
    markdown: string | null;
    build: string | null;
  }>({ dates: null, digest: null, markdown: null, build: null });
  building: boolean = $state(false);
  lastBuildWritten: number | null = $state(null);
  lastUpdatedAt: number | null = $state(null);
  lastQueryDurationMs: number | null = $state(null);
  #datesRead = new LatestRead();
  #digestRead = new LatestRead();
  #markdownRead = new LatestRead();

  get latestDate(): string | null {
    return this.dates[0]?.date ?? null;
  }

  #selectedIndex(): number {
    return this.dates.findIndex((d) => d.date === this.selectedDate);
  }

  get olderDate(): string | null {
    const i = this.#selectedIndex();
    return i >= 0 ? (this.dates[i + 1]?.date ?? null) : null;
  }

  get newerDate(): string | null {
    const i = this.#selectedIndex();
    return i > 0 ? this.dates[i - 1]!.date : null;
  }

  async load(requestedDate: string | null = null): Promise<void> {
    const signal = this.#datesRead.begin();
    const selectedAtStart = this.selectedDate;
    const started = performance.now();
    let status: ReadStatus = "ok";
    this.loading.dates = true;
    this.errors.dates = null;
    try {
      const res = await FrictionService.getApiV1FrictionDigests(undefined, { signal });
      if (!this.#datesRead.isCurrent(signal)) {
        status = "aborted";
        return;
      }
      this.dates = [...(res.digests ?? [])].sort(newestFirst);
      // A date picked while the list was loading wins over the request.
      const wanted = this.selectedDate !== selectedAtStart ? this.selectedDate : requestedDate;
      const target = wanted && this.dates.some((d) => d.date === wanted) ? wanted : this.latestDate;
      if (target === null) {
        this.#digestRead.cancel();
        this.selectedDate = null;
        this.digest = null;
        this.markdown = null;
        this.lastUpdatedAt = Date.now();
      } else {
        await this.selectDate(target);
      }
      this.lastQueryDurationMs = performance.now() - started;
    } catch (e) {
      if (isAbortError(e) || !this.#datesRead.isCurrent(signal)) {
        status = "aborted";
        return;
      }
      status = "error";
      this.errors.dates = errorMessage(e);
    } finally {
      perf.recordPanel({
        route: "friction",
        name: "digests",
        durationMs: performance.now() - started,
        status,
      });
      if (this.#datesRead.finish(signal)) this.loading.dates = false;
    }
  }

  async selectDate(date: string): Promise<void> {
    const signal = this.#digestRead.begin();
    this.#markdownRead.cancel();
    this.selectedDate = date;
    this.markdown = null;
    this.errors.markdown = null;
    this.loading.markdown = false;
    this.loading.digest = true;
    this.errors.digest = null;
    try {
      const digest = await FrictionService.getApiV1FrictionDigestsByDate({ date }, { signal });
      if (!this.#digestRead.isCurrent(signal)) return;
      // Markdown read for the digest this one replaces must not stay on screen.
      this.#markdownRead.cancel();
      this.markdown = null;
      this.loading.markdown = false;
      this.digest = digest;
      this.lastUpdatedAt = Date.now();
    } catch (e) {
      if (isAbortError(e) || !this.#digestRead.isCurrent(signal)) return;
      this.digest = null;
      this.errors.digest = isNotFoundError(e)
        ? m.friction_digest_missing({ date })
        : errorMessage(e);
    } finally {
      if (this.#digestRead.finish(signal)) this.loading.digest = false;
    }
  }

  // loadMarkdown reads the Markdown of the digest on screen, which can lag
  // selectedDate while a newly selected digest loads.
  async loadMarkdown(date: string): Promise<void> {
    const signal = this.#markdownRead.begin();
    this.loading.markdown = true;
    this.errors.markdown = null;
    try {
      const res = await FrictionService.getApiV1FrictionDigestsByDateMd({ date }, { signal });
      const text = await res.text();
      if (!this.#markdownRead.isCurrent(signal) || this.digest?.date !== date) return;
      this.markdown = text;
    } catch (e) {
      if (isAbortError(e) || !this.#markdownRead.isCurrent(signal)) return;
      this.errors.markdown = m.friction_markdown_error();
    } finally {
      if (this.#markdownRead.finish(signal)) this.loading.markdown = false;
    }
  }

  async buildNow(): Promise<void> {
    if (this.building) return;
    this.building = true;
    this.errors.build = null;
    this.lastBuildWritten = null;
    try {
      const res = await FrictionService.postApiV1FrictionRun({});
      const written = (res.reports ?? []).filter((r) => r.written).length;
      this.lastBuildWritten = written;
      await this.load(written > 0 ? null : this.selectedDate);
    } catch (e) {
      this.errors.build = errorMessage(e);
    } finally {
      this.building = false;
    }
  }

  cancelInFlightReads(): void {
    this.#datesRead.cancel();
    this.#digestRead.cancel();
    this.#markdownRead.cancel();
    this.loading.dates = false;
    this.loading.digest = false;
    this.loading.markdown = false;
  }

  reset(): void {
    this.cancelInFlightReads();
    this.dates = [];
    this.selectedDate = null;
    this.digest = null;
    this.markdown = null;
    this.errors.dates = null;
    this.errors.digest = null;
    this.errors.markdown = null;
    this.errors.build = null;
    this.building = false;
    this.lastBuildWritten = null;
    this.lastUpdatedAt = null;
    this.lastQueryDurationMs = null;
  }
}

export const friction = new FrictionStore();
