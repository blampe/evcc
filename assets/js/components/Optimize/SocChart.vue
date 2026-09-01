<template>
	<div>
		<LegendList
			:legends="[{ label: entry.title, color: entry.color, value: '', type: 'area' }]"
		/>
		<div ref="chartEl" class="soc-chart" :class="{ 'soc-chart--labels': showXAxis }"></div>
	</div>
</template>

<script lang="ts">
import { defineComponent, type PropType } from "vue";
import {
	FONT_FAMILY,
	forecastXAxes,
	forecastYAxis,
	tooltipStyle,
	tooltipTable,
} from "../Forecast/echarts";
import colors, { dimColor } from "@/colors";
import formatter from "@/mixins/formatter";
import echartsChart from "@/mixins/echartsChart";
import { is12hFormat } from "@/units";
import type { EvoptData } from "./TimeSeriesDataTable.vue";
import LegendList from "../Sessions/LegendList.vue";
import { slotTimes, disconnectedSlots, expandSlots } from "./chart";

export interface SocChartEntry {
	index: number; // index into evopt.res.batteries
	title: string;
	capacity: number; // kWh
	color: string;
}

export default defineComponent({
	name: "SocChart",
	components: { LegendList },
	mixins: [formatter, echartsChart],
	props: {
		evopt: {
			type: Object as PropType<EvoptData>,
			required: true,
		},
		entry: {
			type: Object as PropType<SocChartEntry>,
			required: true,
		},
		timestamp: {
			type: String,
			default: "",
		},
		showXAxis: { type: Boolean, default: false },
	},
	computed: {
		times(): number[] {
			return slotTimes(this.timestamp, this.evopt.req.time_series.dt);
		},
		endTime(): number {
			const dt = this.evopt.req.time_series.dt;
			const last = this.times[this.times.length - 1];
			return last === undefined ? 0 : last + (dt[dt.length - 1] || 0) * 1000;
		},
		socPercents(): number[] {
			const soc = this.evopt.res.batteries[this.entry.index]?.state_of_charge || [];
			const capacityWh = this.entry.capacity * 1000;
			return this.times.map((_, i) => {
				const wh = soc[i] ?? 0;
				return capacityWh > 0 ? (wh / capacityWh) * 100 : 0;
			});
		},
		// per-slot flag: the car is expected to be disconnected where its per-slot charge
		// cap c_max_t is 0 (see disconnectedSlots).
		disconnected(): boolean[] {
			const cMaxT = this.evopt.req.batteries?.[this.entry.index]?.c_max_t;
			return disconnectedSlots(cMaxT, this.times.length);
		},
		hasDisconnected(): boolean {
			return this.disconnected.some(Boolean);
		},
		xAxes(): Record<string, unknown>[] {
			const stepHours = is12hFormat() ? 6 : 4;
			const [hourAxis, dayAxis] = forecastXAxes(
				this.times[0] ?? 0,
				this.endTime,
				this.hourShort,
				this.weekdayShort,
				stepHours
			);
			if (!this.showXAxis) {
				(hourAxis as Record<string, unknown>)["axisLabel"] = { show: false };
			}
			return [hourAxis, dayAxis];
		},
		chartOption(): Record<string, unknown> {
			return {
				animation: false,
				textStyle: { fontFamily: FONT_FAMILY },
				grid: {
					top: 10,
					right: 36,
					bottom: this.showXAxis ? 34 : 8,
					left: 0,
					borderWidth: 0,
				},
				tooltip: {
					trigger: "axis",
					axisPointer: {
						type: "line",
						lineStyle: { color: colors.muted || "", opacity: 0.4 },
					},
					...tooltipStyle(colors.text || "", () => this.chart),
					formatter: this.tooltipFormatter,
				},
				xAxis: this.xAxes,
				yAxis: forecastYAxis({
					min: 0,
					max: 100,
					position: "right",
					interval: 50,
					axisLabel: {
						color: colors.muted || "",
						formatter: (v: number) => this.fmtNumber(v, 0),
					},
				}),
				series: this.socChartSeries,
			};
		},
		socChartSeries(): Record<string, unknown>[] {
			const color = this.entry.color;
			const muted = colors.muted || "";

			const line = (
				data: [number, number | null][],
				c: string,
				z: number,
				silent = false
			) => ({
				type: "line",
				z,
				data,
				showSymbol: false,
				silent,
				lineStyle: { color: c, width: 2 },
				itemStyle: { color: c },
				areaStyle: { color: dimColor(c) },
				emphasis: { disabled: true },
			});

			const expand = (values: (number | null)[]) => expandSlots(this.times, this.endTime, values);

			// colored layer, drawn in full when the car never disconnects
			if (!this.hasDisconnected) {
				return [line(expand(this.socPercents), color, 3)];
			}

			// grey base across the whole horizon greys the projection where the car is
			// expected to be disconnected; the colored layer is masked to the connected
			// slots and drawn on top, so only those windows keep the battery colour
			const masked = this.socPercents.map((v, i) => (this.disconnected[i] ? null : v));
			return [line(expand(this.socPercents), muted, 2, true), line(expand(masked), color, 3)];
		},
	},
	methods: {
		tooltipFormatter(params: { axisValue: number; value?: [number, number] }[]): string {
			const arr = Array.isArray(params) ? params : [params];
			const v = arr[0]?.value?.[1];
			if (v == null || Number.isNaN(v)) return "";
			const t = new Date(arr[0]!.axisValue);
			const head = `${this.weekdayShort(t)} ${this.fmtHourMinute(t)}`;
			return tooltipTable(head, [{ values: [this.fmtPercentage(v)] }]);
		},
	},
});
</script>

<style scoped>
.soc-chart {
	height: 54px;
	width: 100%;
}
.soc-chart--labels {
	height: 80px;
}
</style>
