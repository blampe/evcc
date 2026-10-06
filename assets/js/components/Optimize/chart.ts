import { xAxisLabelStyle } from "../Forecast/echarts";
import colors from "@/colors";
import type { BatteryDetail } from "@/types/evcc";

// loadpoint part of a vehicle entry title: "Carport (blue e-Golf)" → "Carport"
export function loadpointTitle(detail: BatteryDetail): string {
  const title = detail.title || detail.name || "";
  return title.replace(/\s*\([^)]*\)$/, "");
}

// slot start times (ms) from the first timestamp plus cumulative dt seconds
export function slotTimes(timestamp: string, dt: number[]): number[] {
  const start = new Date(timestamp).getTime();
  const res: number[] = [];
  let acc = 0;
  for (const s of dt) {
    res.push(start + acc * 1000);
    acc += s;
  }
  return res;
}

export function isMidnight(time?: number): boolean {
  if (time === undefined) return false;
  const d = new Date(time);
  return d.getHours() === 0 && d.getMinutes() === 0;
}

// category x axis over slot times, labels at full hours every 4h (6h on mobile),
// day boundaries as split line with the weekday shown at 00:00
export function slotXAxis(times: number[], weekdayShort: (d: Date) => string) {
  const step = window.innerWidth < 576 ? 6 : 4;
  return {
    type: "category",
    data: times.map(String),
    axisLine: { show: false },
    axisTick: { show: false },
    splitLine: {
      show: true,
      // split lines sit at the left edge of their slot
      interval: (index: number) => index > 0 && isMidnight(times[index]),
      lineStyle: { color: colors.muted || "", type: "solid" },
    },
    axisLabel: {
      ...xAxisLabelStyle(),
      interval: 0,
      formatter: (value: string) => {
        const d = new Date(Number(value));
        if (d.getMinutes() !== 0 || d.getHours() % step !== 0) return "";
        const label = String(d.getHours());
        return d.getHours() === 0 ? `${label}\n${weekdayShort(d)}` : label;
      },
    },
  };
}

export function formatSlotRange(times: number[], dt: number[], index: number): string {
  const start = new Date(times[index] ?? 0);
  const end = new Date(start.getTime() + (dt[index] || 0) * 1000);
  const f = (d: Date) =>
    `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
  return `${f(start)} - ${f(end)}`;
}

// slot energy (Wh) to average power (kW)
export function whToKW(wh: number, dtSeconds: number): number {
  return wh / (dtSeconds / 3600) / 1000;
}

// per-slot net grid power (kW): import positive, export negative. Nets rather than
// picking "import if nonzero": the solver leaves float noise (e.g. 1e-28 Wh) in the
// idle direction, which would otherwise mask a real export.
export function netGridKW(gridImport: number[], gridExport: number[], dt: number[]): number[] {
  return gridImport.map((imp, i) => whToKW(imp - (gridExport[i] || 0), dt[i] || 0));
}

// per-slot flag: the car is expected to be disconnected in slots where its per-slot
// charge cap c_max_t is 0. An absent c_max_t (e.g. a home battery) is never disconnected.
export function disconnectedSlots(cMaxT: number[] | undefined, length: number): boolean[] {
  return Array.from({ length }, (_, i) => Array.isArray(cMaxT) && cMaxT[i] === 0);
}

// expandSlots turns per-slot values into an explicit staircase - two points per slot,
// [slotStart, v] and [slotEnd, v] - so a plain line renders the step:"start" shape while
// null slots break cleanly. That lets a colour layer masked to the connected slots sit over
// a continuous grey base without the step/null rendering ambiguities of a single stepped
// series. The last slot ends at endTime.
export function expandSlots(
  times: number[],
  endTime: number,
  values: (number | null)[]
): [number, number | null][] {
  const out: [number, number | null][] = [];
  for (let i = 0; i < times.length; i++) {
    const end = i + 1 < times.length ? times[i + 1]! : endTime;
    const v = values[i] ?? null;
    out.push([times[i]!, v]);
    out.push([end, v]);
  }
  return out;
}

// netGridCost is the plan's money with the grid: what the imported energy costs minus
// what the exported energy earns, over the whole horizon (positive = you pay). Energy
// stored in the batteries at the end is left out on purpose - the optimizer's
// objective_value credits it, which also counts a car's charge as value and made a
// plan with no import at all read as a cost once the car drove more than it recharged.
export function netGridCost(
  gridImport: number[] | undefined,
  gridExport: number[] | undefined,
  priceImport: number[] | undefined,
  priceExport: number[] | undefined
): number {
  let cost = 0;
  (gridImport || []).forEach((wh, i) => (cost += wh * (priceImport?.[i] || 0)));
  (gridExport || []).forEach((wh, i) => (cost -= wh * (priceExport?.[i] || 0)));
  return cost;
}
