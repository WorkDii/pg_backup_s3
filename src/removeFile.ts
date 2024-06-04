import { $ } from "zx";
import { env } from "./env.js";

export const removeLocalFile = async (database: string) => {
  await $`rm -rf ${database}.gz`;
};
