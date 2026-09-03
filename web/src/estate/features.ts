import {
  createSortedRowModel,
  rowSortingFeature,
  sortFns,
  tableFeatures,
} from "@tanstack/react-table";

/**
 * The table's feature set, declared once.
 *
 * TanStack Table v9 is opt-in: a table gets only the features it registers, and the registered set
 * is a *type* that column definitions and the table instance both have to agree on. Declaring it
 * here and threading `typeof estateFeatures` through both is what keeps the column definitions
 * type-safe against the row.
 *
 * Sorting only. Row expansion is a boolean per row in this screen — not a tree, not a grouped row
 * model — so it is a `useState` in the component rather than a feature and a row model, and
 * pagination and filtering are not registered because an estate view that pages or hides rows is
 * not answering the question it exists for.
 */
export const estateFeatures = tableFeatures({
  rowSortingFeature,
  sortFns,
  sortedRowModel: createSortedRowModel(),
});
