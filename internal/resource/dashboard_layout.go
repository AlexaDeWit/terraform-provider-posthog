package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/posthog/terraform-provider/internal/httpclient"
	"github.com/posthog/terraform-provider/internal/resource/core"
)

type TileTFModel struct {
	TileID      types.Int64          `tfsdk:"tile_id"`
	InsightID   types.Int64          `tfsdk:"insight_id"`
	TextBody    types.String         `tfsdk:"text_body"`
	Color       types.String         `tfsdk:"color"`
	LayoutsJSON jsontypes.Normalized `tfsdk:"layouts_json"`
}

func (t TileTFModel) IsInsightTile() bool {
	return !t.InsightID.IsNull() && !t.InsightID.IsUnknown()
}

func (t TileTFModel) IsTextTile() bool {
	return !t.TextBody.IsNull() && !t.TextBody.IsUnknown()
}

type DashboardLayoutTFModel struct {
	core.BaseInt64Identifiable
	core.BaseProjectID
	DashboardID types.Int64 `tfsdk:"dashboard_id"`
	Tiles       types.List  `tfsdk:"tiles"` // element type: TileTFModel
}

var tilesObjectType = types.ObjectType{
	AttrTypes: map[string]attr.Type{
		"tile_id":      types.Int64Type,
		"insight_id":   types.Int64Type,
		"text_body":    types.StringType,
		"color":        types.StringType,
		"layouts_json": jsontypes.NormalizedType{},
	},
}

type DashboardLayoutOps struct {
	// planTiles is set by BuildUpdateRequest and consumed by Update so that the
	// reconciled tile list from the plan is available during the PATCH call.
	planTiles []TileTFModel
}

func NewDashboardLayout() resource.Resource {
	return core.NewGenericResource[DashboardLayoutTFModel, httpclient.DashboardLayoutPatchRequest, httpclient.DashboardLayoutResponse](
		&DashboardLayoutOps{},
		core.ProjectScopedImportParser[DashboardLayoutTFModel](),
	)
}

func (o *DashboardLayoutOps) ResourceName() string {
	return "dashboard_layout"
}

func (o *DashboardLayoutOps) Schema() schema.Schema {
	return schema.Schema{
		MarkdownDescription: "Manages the tile layout of a PostHog dashboard. This resource is fully authoritative: it manages all tile layouts on the dashboard. Tiles not declared in config will have their layouts cleared on apply.",
		Attributes: map[string]schema.Attribute{
			"id": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Resource ID (same value as dashboard_id).",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"project_id": core.ProjectIDSchemaAttribute(),
			"dashboard_id": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "PostHog dashboard ID.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"tiles": schema.ListNestedAttribute{
				Required:            true,
				MarkdownDescription: "Ordered list of tiles to manage on the dashboard.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"tile_id": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Server-assigned tile ID. Populated after the first apply.",
							PlanModifiers: []planmodifier.Int64{
								int64planmodifier.UseStateForUnknown(),
							},
						},
						"insight_id": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "ID of the insight to display. Exactly one of insight_id or text_body must be set.",
							Validators: []validator.Int64{
								int64validator.ExactlyOneOf(
									path.MatchRelative().AtParent().AtName("text_body"),
								),
							},
						},
						"text_body": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "Markdown body for a text tile (max 4000 characters). Exactly one of insight_id or text_body must be set.",
							Validators: []validator.String{
								stringvalidator.LengthAtMost(4000),
							},
						},
						"color": schema.StringAttribute{
							Optional:            true,
							MarkdownDescription: "Background color of the tile. Valid values are defined by the PostHog API; see [InsightColor in types.ts](https://github.com/PostHog/posthog/blob/master/frontend/src/types.ts#L2154) for the current list.",
						},
						"layouts_json": schema.StringAttribute{
							Optional:            true,
							Computed:            true,
							CustomType:          jsontypes.NormalizedType{},
							MarkdownDescription: "JSON object with breakpoint keys `sm` and/or `xs`, each containing position properties: `x`, `y`, `w`, `h` (required), and optionally `minW`, `minH` (e.g. `{\"sm\":{\"x\":0,\"y\":0,\"w\":6,\"h\":5},\"xs\":{\"x\":0,\"y\":0,\"w\":1,\"h\":5}}`). Semantic JSON equality is used to suppress phantom diffs.",
							PlanModifiers: []planmodifier.String{
								stringplanmodifier.UseStateForUnknown(),
							},
						},
					},
				},
			},
		},
	}
}

func (o *DashboardLayoutOps) BuildCreateRequest(_ context.Context, _ DashboardLayoutTFModel) (httpclient.DashboardLayoutPatchRequest, diag.Diagnostics) {
	// BuildCreateRequest returns an empty request because Create performs its own
	// GET-reconcile-PATCH flow using the HTTP client directly.
	return httpclient.DashboardLayoutPatchRequest{}, nil
}

func (o *DashboardLayoutOps) BuildUpdateRequest(ctx context.Context, plan, _ DashboardLayoutTFModel) (httpclient.DashboardLayoutPatchRequest, diag.Diagnostics) {
	tiles, diags := extractTilesFromModel(ctx, plan)
	if diags.HasError() {
		return httpclient.DashboardLayoutPatchRequest{}, diags
	}
	o.planTiles = tiles
	return httpclient.DashboardLayoutPatchRequest{}, diags
}

func (o *DashboardLayoutOps) MapResponseToModel(ctx context.Context, resp httpclient.DashboardLayoutResponse, model *DashboardLayoutTFModel) diag.Diagnostics {
	var diags diag.Diagnostics

	// Set ID and DashboardID from response. ID is required for GenericResource to function;
	// DashboardID is set here to handle the import case where it was not provided by the import parser.
	model.ID = types.Int64Value(resp.ID)
	model.DashboardID = types.Int64Value(resp.ID)

	configTiles, d := extractTilesFromModel(ctx, *model)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}

	stateTiles, d := mapTilesToState(resp.Tiles, configTiles)
	diags.Append(d...)

	if stateTiles == nil {
		stateTiles = []TileTFModel{}
	}

	tilesList, d := types.ListValueFrom(ctx, tilesObjectType, stateTiles)
	diags.Append(d...)

	model.Tiles = tilesList
	return diags
}

func (o *DashboardLayoutOps) Create(ctx context.Context, client httpclient.PosthogClient, model DashboardLayoutTFModel, _ httpclient.DashboardLayoutPatchRequest) (httpclient.DashboardLayoutResponse, error) {
	declaredTiles, diags := extractTilesFromModel(ctx, model)
	if diags.HasError() {
		return httpclient.DashboardLayoutResponse{}, fmt.Errorf("extracting tiles from config: %s", diags.Errors()[0].Detail())
	}

	resp, _, err := reconcileAndPatch(ctx, &client, model.GetEffectiveProjectID(), model.DashboardID.ValueInt64(), declaredTiles)
	return resp, err
}

func (o *DashboardLayoutOps) Read(ctx context.Context, client httpclient.PosthogClient, model DashboardLayoutTFModel) (httpclient.DashboardLayoutResponse, httpclient.HTTPStatusCode, error) {
	projectID := model.GetEffectiveProjectID()

	// Import fallback: during import, DashboardID may not be set yet. Fall back to the
	// resource ID (which is set by the import parser from the last path segment).
	dashboardID := strconv.FormatInt(model.DashboardID.ValueInt64(), 10)
	if model.DashboardID.IsNull() || model.DashboardID.IsUnknown() || model.DashboardID.ValueInt64() == 0 {
		dashboardID = model.GetID()
	}

	c := &client
	return c.GetDashboardLayout(ctx, projectID, dashboardID)
}

func (o *DashboardLayoutOps) Update(ctx context.Context, client httpclient.PosthogClient, model DashboardLayoutTFModel, _ httpclient.DashboardLayoutPatchRequest) (httpclient.DashboardLayoutResponse, httpclient.HTTPStatusCode, error) {
	return reconcileAndPatch(ctx, &client, model.GetEffectiveProjectID(), model.DashboardID.ValueInt64(), o.planTiles)
}

// reconcileAndPatch performs a GET-reconcile-PATCH flow.
// It reads the current dashboard state, correlates declared tiles with API tiles,
// re-associates any missing insights, and applies the resulting patch.
func reconcileAndPatch(ctx context.Context, client *httpclient.PosthogClient, projectID string, dashboardIDInt int64, declaredTiles []TileTFModel) (httpclient.DashboardLayoutResponse, httpclient.HTTPStatusCode, error) {
	dashboardID := strconv.FormatInt(dashboardIDInt, 10)

	apiResp, statusCode, err := client.GetDashboardLayout(ctx, projectID, dashboardID)
	if err != nil {
		return httpclient.DashboardLayoutResponse{}, statusCode, fmt.Errorf("reading dashboard %s: %w", dashboardID, err)
	}

	patchItems, missingInsightIDs := buildLayoutPatch(ctx, declaredTiles, apiResp.Tiles)

	if len(missingInsightIDs) > 0 {
		tflog.Info(ctx, "Re-associating insights removed from dashboard", map[string]any{
			"insight_ids":  missingInsightIDs,
			"dashboard_id": dashboardID,
		})

		for _, insightID := range missingInsightIDs {
			if err := reAssociateInsight(ctx, client, projectID, dashboardIDInt, insightID); err != nil {
				return httpclient.DashboardLayoutResponse{}, 0, fmt.Errorf("re-associating insight %d with dashboard %s: %w", insightID, dashboardID, err)
			}
		}

		// Re-GET after re-association so the new tile entries are visible.
		apiResp, statusCode, err = client.GetDashboardLayout(ctx, projectID, dashboardID)
		if err != nil {
			return httpclient.DashboardLayoutResponse{}, statusCode, fmt.Errorf("reading dashboard %s after re-association: %w", dashboardID, err)
		}

		patchItems, missingInsightIDs = buildLayoutPatch(ctx, declaredTiles, apiResp.Tiles)
		if len(missingInsightIDs) > 0 {
			return httpclient.DashboardLayoutResponse{}, 0, fmt.Errorf("insights %v still not found on dashboard %s after re-association", missingInsightIDs, dashboardID)
		}
	}

	if len(patchItems) == 0 {
		return apiResp, 200, nil
	}

	return client.UpdateDashboardLayout(ctx, projectID, dashboardID, httpclient.DashboardLayoutPatchRequest{Tiles: patchItems})
}

func (o *DashboardLayoutOps) Delete(ctx context.Context, client httpclient.PosthogClient, model DashboardLayoutTFModel) (httpclient.HTTPStatusCode, error) {
	declaredTiles, diags := extractTilesFromModel(ctx, model)
	if diags.HasError() {
		return 0, fmt.Errorf("extracting tiles from state for delete: %s", diags.Errors()[0].Detail())
	}

	projectID := model.GetEffectiveProjectID()
	dashboardID := strconv.FormatInt(model.DashboardID.ValueInt64(), 10)

	c := &client

	apiResp, statusCode, err := c.GetDashboardLayout(ctx, projectID, dashboardID)
	if err != nil {
		return statusCode, fmt.Errorf("reading dashboard %s for delete: %w", dashboardID, err)
	}

	patchItems := buildDeletePatch(declaredTiles, apiResp.Tiles)
	if len(patchItems) == 0 {
		return 200, nil
	}

	_, statusCode, err = c.UpdateDashboardLayout(ctx, projectID, dashboardID, httpclient.DashboardLayoutPatchRequest{Tiles: patchItems})
	return statusCode, err
}

// extractTilesFromModel extracts tiles from a DashboardLayoutTFModel.
func extractTilesFromModel(ctx context.Context, model DashboardLayoutTFModel) ([]TileTFModel, diag.Diagnostics) {
	if model.Tiles.IsNull() || model.Tiles.IsUnknown() {
		return nil, nil
	}
	var tiles []TileTFModel
	diags := model.Tiles.ElementsAs(ctx, &tiles, false)
	return tiles, diags
}

// buildTileLookupMaps indexes API tiles into two maps: insight tiles keyed by insight ID,
// and text tiles keyed by tile ID.
func buildTileLookupMaps(apiTiles []httpclient.DashboardTile) (insightByID, textByTileID map[int64]httpclient.DashboardTile) {
	insightByID = make(map[int64]httpclient.DashboardTile, len(apiTiles))
	textByTileID = make(map[int64]httpclient.DashboardTile, len(apiTiles))
	for _, t := range apiTiles {
		if t.Insight != nil {
			insightByID[t.Insight.ID] = t
		}
		if t.Text != nil {
			textByTileID[t.ID] = t
		}
	}
	return insightByID, textByTileID
}

// buildLayoutPatch builds the set of tile patch items for a PATCH request.
// It correlates declared config tiles with API tiles, and enforces authoritative
// behavior by clearing layouts on any unmanaged tile that has existing layouts.
// Returns the patch items and a list of insight IDs that were declared but not found in the API.
func buildLayoutPatch(ctx context.Context, declaredTiles []TileTFModel, apiTiles []httpclient.DashboardTile) ([]httpclient.DashboardTilePatchItem, []int64) {
	insightTileMap, textTileMap := buildTileLookupMaps(apiTiles)

	matchedTileIDs := make(map[int64]bool)
	var patchItems []httpclient.DashboardTilePatchItem
	var missingInsightIDs []int64

	for _, declared := range declaredTiles {
		if declared.IsInsightTile() {
			insightID := declared.InsightID.ValueInt64()
			if apiTile, ok := insightTileMap[insightID]; ok {
				matchedTileIDs[apiTile.ID] = true
				patchItems = append(patchItems, buildTilePatchItem(ctx, apiTile.ID, declared))
			} else {
				missingInsightIDs = append(missingInsightIDs, insightID)
			}
		} else if declared.IsTextTile() {
			tileID := declared.TileID.ValueInt64()
			if apiTile, ok := textTileMap[tileID]; ok {
				matchedTileIDs[apiTile.ID] = true
				patchItems = append(patchItems, buildTilePatchItem(ctx, apiTile.ID, declared))
			} else {
				// New text tile (tile_id is zero/unknown on first apply).
				patchItems = append(patchItems, buildNewTextTilePatchItem(ctx, declared))
			}
		}
	}

	// Authoritative enforcement: clear layouts on unmanaged tiles.
	for _, t := range apiTiles {
		if !matchedTileIDs[t.ID] && len(t.Layouts) > 0 {
			emptyLayouts := map[string]interface{}{}
			patchItems = append(patchItems, httpclient.DashboardTilePatchItem{
				ID:      t.ID,
				Layouts: &emptyLayouts,
			})
		}
	}

	return patchItems, missingInsightIDs
}

// buildDeletePatch builds the set of tile patch items to reset/delete tiles when the
// resource is destroyed. Insight tiles have their layouts cleared; text tiles are soft-deleted.
func buildDeletePatch(declaredTiles []TileTFModel, apiTiles []httpclient.DashboardTile) []httpclient.DashboardTilePatchItem {
	insightTileMap, textTileMap := buildTileLookupMaps(apiTiles)

	var patchItems []httpclient.DashboardTilePatchItem
	for _, declared := range declaredTiles {
		if declared.IsInsightTile() {
			insightID := declared.InsightID.ValueInt64()
			if apiTile, ok := insightTileMap[insightID]; ok {
				emptyLayouts := map[string]interface{}{}
				patchItems = append(patchItems, httpclient.DashboardTilePatchItem{
					ID:      apiTile.ID,
					Layouts: &emptyLayouts,
				})
			}
		} else if declared.IsTextTile() {
			tileID := declared.TileID.ValueInt64()
			if apiTile, ok := textTileMap[tileID]; ok {
				deleted := true
				patchItems = append(patchItems, httpclient.DashboardTilePatchItem{
					ID:      apiTile.ID,
					Deleted: &deleted,
				})
			}
		}
	}

	return patchItems
}

// buildTilePatchItem constructs a DashboardTilePatchItem from a declared tile and a known API tile ID.
func buildTilePatchItem(ctx context.Context, tileID int64, declared TileTFModel) httpclient.DashboardTilePatchItem {
	item := httpclient.DashboardTilePatchItem{
		ID: tileID,
	}

	if !declared.LayoutsJSON.IsNull() && !declared.LayoutsJSON.IsUnknown() {
		var layouts map[string]interface{}
		if err := json.Unmarshal([]byte(declared.LayoutsJSON.ValueString()), &layouts); err == nil {
			item.Layouts = &layouts
		} else {
			tflog.Warn(ctx, "Failed to unmarshal layouts_json; leaving layouts unset", map[string]any{
				"error":        err.Error(),
				"layouts_json": declared.LayoutsJSON.ValueString(),
			})
		}
	}

	if !declared.Color.IsNull() {
		c := declared.Color.ValueString()
		item.Color = &c
	}

	if declared.IsTextTile() {
		item.Text = &httpclient.DashboardTileTextPatch{Body: declared.TextBody.ValueString()}
	}

	return item
}

// buildNewTextTilePatchItem constructs a patch item for a new text tile.
// ID=0 is omitted from the JSON payload by omitempty, which tells the API to create a new tile.
func buildNewTextTilePatchItem(ctx context.Context, declared TileTFModel) httpclient.DashboardTilePatchItem {
	return buildTilePatchItem(ctx, 0, declared)
}

// apiTileToTFModel converts a single API tile to the Terraform model representation.
func apiTileToTFModel(t httpclient.DashboardTile) TileTFModel {
	tile := TileTFModel{
		TileID: types.Int64Value(t.ID),
	}

	if t.Insight != nil {
		tile.InsightID = types.Int64Value(t.Insight.ID)
		tile.TextBody = types.StringNull()
	} else if t.Text != nil {
		tile.InsightID = types.Int64Null()
		tile.TextBody = types.StringValue(t.Text.Body)
	}

	if t.Color != nil && *t.Color != "" {
		tile.Color = types.StringValue(*t.Color)
	} else {
		tile.Color = types.StringNull()
	}

	if len(t.Layouts) > 0 {
		bytes, err := json.Marshal(t.Layouts)
		if err == nil {
			tile.LayoutsJSON = jsontypes.NewNormalizedValue(string(bytes))
		} else {
			tile.LayoutsJSON = jsontypes.NewNormalizedNull()
		}
	} else {
		tile.LayoutsJSON = jsontypes.NewNormalizedNull()
	}

	return tile
}

// mapTilesToState maps API tiles to Terraform state, preserving config order to avoid
// phantom diffs from ListNestedAttribute positional comparison.
func mapTilesToState(apiTiles []httpclient.DashboardTile, configTiles []TileTFModel) ([]TileTFModel, diag.Diagnostics) {
	if configTiles == nil {
		// Import case: return all tiles in API order.
		result := make([]TileTFModel, 0, len(apiTiles))
		for _, t := range apiTiles {
			result = append(result, apiTileToTFModel(t))
		}
		return result, nil
	}

	insightTileMap, textTileMap := buildTileLookupMaps(apiTiles)

	// Iterate config tiles in config order to maintain stable ordering.
	var result []TileTFModel
	for _, config := range configTiles {
		if config.IsInsightTile() {
			if apiTile, ok := insightTileMap[config.InsightID.ValueInt64()]; ok {
				result = append(result, apiTileToTFModel(apiTile))
			}
		} else if config.IsTextTile() {
			if apiTile, ok := textTileMap[config.TileID.ValueInt64()]; ok {
				result = append(result, apiTileToTFModel(apiTile))
			}
		}
		// Tile not found in API: omit from state (will trigger recreation on next plan).
	}

	return result, nil
}

// reAssociateInsight ensures an insight is associated with the given dashboard.
// It GETs the current insight, checks if the dashboard is already in its list,
// and if not, appends the dashboard ID and PATCHes the insight.
// This preserves all existing dashboard associations.
func reAssociateInsight(ctx context.Context, client *httpclient.PosthogClient, projectID string, dashboardID int64, insightID int64) error {
	insightIDStr := strconv.FormatInt(insightID, 10)

	insight, _, err := client.GetInsight(ctx, projectID, insightIDStr)
	if err != nil {
		return fmt.Errorf("getting insight %s: %w", insightIDStr, err)
	}

	// Check if already associated with this dashboard.
	for _, d := range insight.Dashboards {
		if d == int32(dashboardID) {
			return nil // Already on this dashboard.
		}
	}

	dashboards := append(insight.Dashboards, int32(dashboardID))
	_, _, err = client.UpdateInsight(ctx, projectID, insightIDStr, httpclient.InsightRequest{
		Dashboards: dashboards,
	})
	return err
}
