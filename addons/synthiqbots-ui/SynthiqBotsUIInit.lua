-- MULTIBAR --

local tMultiBar = SynthiqBotsUI.addFrame("MultiBar", -262, 144, 36)
tMultiBar:SetMovable(true)

-- LEFT --

local tLeft = tMultiBar.addFrame("Left", -76, 2, 32)

-- TANKER --

tLeft.addButton("Tanker", -170, 0, "ability_warrior_shieldbash", SynthiqBotsUI.tips.tanker.master)
.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("@tank do attack my target") end
end

-- ATTACK --

local tButton = tLeft.addButton("Attack", -136, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\attack.blp", SynthiqBotsUI.tips.attack.master)
tButton.doRight = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Attack"])
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("do attack my target") end
end

local tAttack = tLeft.addFrame("Attack", -138, 34)
tAttack:Hide()

local tButton = tAttack.addButton("Attack", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\attack.blp", SynthiqBotsUI.tips.attack.attack)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButtonWithTarget(pButton.parent.parent, "Attack", pButton.texture, "do attack my target")
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("do attack my target") end
end

local tButton = tAttack.addButton("Ranged", 0, 30, "Interface\\AddOns\\synthiqbots-ui\\Icons\\attack_ranged.blp", SynthiqBotsUI.tips.attack.ranged)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButtonWithTarget(pButton.parent.parent, "Attack", pButton.texture, "@ranged do attack my target")
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("@ranged do attack my target") end
end

local tButton = tAttack.addButton("Melee", 0, 60, "Interface\\AddOns\\synthiqbots-ui\\Icons\\attack_melee.blp", SynthiqBotsUI.tips.attack.melee)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButtonWithTarget(pButton.parent.parent, "Attack", pButton.texture, "@melee do attack my target")
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("@melee do attack my target") end
end

local tButton = tAttack.addButton("Healer", 0, 90, "Interface\\AddOns\\synthiqbots-ui\\Icons\\attack_healer.blp", SynthiqBotsUI.tips.attack.healer)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButtonWithTarget(pButton.parent.parent, "Attack", pButton.texture, "@healer do attack my target")
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("@healer do attack my target") end
end

local tButton = tAttack.addButton("Dps", 0, 120, "Interface\\AddOns\\synthiqbots-ui\\Icons\\attack_dps.blp", SynthiqBotsUI.tips.attack.dps)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButtonWithTarget(pButton.parent.parent, "Attack", pButton.texture, "@dps do attack my target")
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("@dps do attack my target") end
end

local tButton = tAttack.addButton("Tank", 0, 150, "Interface\\AddOns\\synthiqbots-ui\\Icons\\attack_tank.blp", SynthiqBotsUI.tips.attack.tank)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButtonWithTarget(pButton.parent.parent, "Attack", pButton.texture, "@tank do attack my target")
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.isTarget()) then SynthiqBotsUI.ActionToGroup("@tank do attack my target") end
end

-- MODE --

local tButton = tLeft.addButton("Mode", -102, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\mode_passive.blp", SynthiqBotsUI.tips.mode.master).setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Mode"])
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.ActionToGroup("co +passive,?")
	else
		SynthiqBotsUI.ActionToGroup("co -passive,?")
	end
end

local tMode = tLeft.addFrame("Mode", -104, 34)
tMode:Hide()

tMode.addButton("Passive", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\mode_passive.blp", SynthiqBotsUI.tips.mode.passive)
.doLeft = function(pButton)
	if(SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Mode", pButton.texture, "co +passive,?")) then
		pButton.parent.parent.buttons["Mode"].setEnable().doLeft = function(pButton)
			if(SynthiqBotsUI.OnOffSwitch(pButton)) then
				SynthiqBotsUI.ActionToGroup("co +passive,?")
			else
				SynthiqBotsUI.ActionToGroup("co -passive,?")
			end
		end
	end
end

tMode.addButton("Grind", 0, 30, "Interface\\AddOns\\synthiqbots-ui\\Icons\\mode_grind.blp", SynthiqBotsUI.tips.mode.grind)
.doLeft = function(pButton)
	if(SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Mode", pButton.texture, "grind")) then
		pButton.parent.parent.buttons["Mode"].setEnable().doLeft = function(pButton)
			if(SynthiqBotsUI.OnOffSwitch(pButton)) then
				SynthiqBotsUI.ActionToGroup("grind")
			else
				SynthiqBotsUI.ActionToGroup("follow")
			end
		end
	end
end

-- STAY|FOLLOW --

tLeft.addButton("Stay", -68, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\command_follow.blp", SynthiqBotsUI.tips.stallow.stay)
.doLeft = function(pButton)
	if(SynthiqBotsUI.ActionToGroup("stay")) then
		pButton.parent.buttons["Follow"].doShow()
		pButton.parent.buttons["ExpandFollow"].setDisable()
		pButton.parent.buttons["ExpandStay"].setEnable()
		pButton.doHide()
	end
end

tLeft.addButton("Follow", -68, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\command_stay.blp", SynthiqBotsUI.tips.stallow.follow).doHide()
.doLeft = function(pButton)
	if(SynthiqBotsUI.ActionToGroup("follow")) then
		pButton.parent.buttons["Stay"].doShow()
		pButton.parent.buttons["ExpandFollow"].setEnable()
		pButton.parent.buttons["ExpandStay"].setDisable()
		pButton.doHide()
	end
end

tLeft.addButton("ExpandStay", -68, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\command_stay.blp", SynthiqBotsUI.tips.expand.stay).doHide().setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("stay")
	pButton.parent.buttons["ExpandFollow"].setDisable()
	pButton.setEnable()
end

tLeft.addButton("ExpandFollow", -102, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\command_follow.blp", SynthiqBotsUI.tips.expand.follow).doHide()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("follow")
	pButton.parent.buttons["ExpandStay"].setDisable()
	pButton.setEnable()
end

-- FLEE --

local tButton = tLeft.addButton("Flee", -34, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee.blp", SynthiqBotsUI.tips.flee.master)
tButton.doRight = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Flee"])
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("flee")
end

local tFlee = tLeft.addFrame("Flee", -36, 34)
tFlee:Hide()

local tButton = tFlee.addButton("Flee", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee.blp", SynthiqBotsUI.tips.flee.flee)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButton(pButton.parent.parent, "Flee", pButton.texture, "flee")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("flee")
end

local tButton = tFlee.addButton("Ranged", 0, 30, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee_ranged.blp", SynthiqBotsUI.tips.flee.ranged)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButton(pButton.parent.parent, "Flee", pButton.texture, "@ranged flee")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@ranged flee")
end

local tButton = tFlee.addButton("Melee", 0, 60, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee_melee.blp", SynthiqBotsUI.tips.flee.melee)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButton(pButton.parent.parent, "Flee", pButton.texture, "@melee flee")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@melee flee")
end

local tButton = tFlee.addButton("Healer", 0, 90, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee_healer.blp", SynthiqBotsUI.tips.flee.healer)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButton(pButton.parent.parent, "Flee", pButton.texture, "@healer flee")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@healer flee")
end

local tButton = tFlee.addButton("Dps", 0, 120, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee_dps.blp", SynthiqBotsUI.tips.flee.dps)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButton(pButton.parent.parent, "Flee", pButton.texture, "@dps flee")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@dps flee")
end

local tButton = tFlee.addButton("Tank", 0, 150, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee_tank.blp", SynthiqBotsUI.tips.flee.tank)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToGroupButton(pButton.parent.parent, "Flee", pButton.texture, "@tank flee")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@tank flee")
end

local tButton = tFlee.addButton("Target", 0, 180, "Interface\\AddOns\\synthiqbots-ui\\Icons\\flee_target.blp", SynthiqBotsUI.tips.flee.target)
tButton.doRight = function(pButton)
	SynthiqBotsUI.SelectToTargetButton(pButton.parent.parent, "Flee", pButton.texture, "flee")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTarget("flee")
end

-- FORMATION --

local tButton = tLeft.addButton("Format", -0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_near.blp", SynthiqBotsUI.tips.format.master)
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("formation")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Format"])
end

local tFormat = tLeft.addFrame("Format", -2, 34)
tFormat:Hide()

tFormat.addButton("Arrow", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_arrow.blp", SynthiqBotsUI.tips.format.arrow)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation arrow")
end

tFormat.addButton("Queue", 0, 30, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_queue.blp", SynthiqBotsUI.tips.format.queue)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation queue")
end

tFormat.addButton("Near", 0, 60, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_near.blp", SynthiqBotsUI.tips.format.near)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation near")
end

tFormat.addButton("Melee", 0, 90, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_melee.blp", SynthiqBotsUI.tips.format.melee)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation melee")
end

tFormat.addButton("Line", 0, 120, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_line.blp", SynthiqBotsUI.tips.format.line)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation line")
end

tFormat.addButton("Circle", 0, 150, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_circle.blp", SynthiqBotsUI.tips.format.circle)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation circle")
end

tFormat.addButton("Chaos", 0, 180, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_chaos.blp", SynthiqBotsUI.tips.format.chaos)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation chaos")
end

tFormat.addButton("Shield", 0, 210, "Interface\\AddOns\\synthiqbots-ui\\Icons\\formation_shield.blp", SynthiqBotsUI.tips.format.shield)
.doLeft = function(pButton)
	SynthiqBotsUI.SelectToGroup(pButton.parent.parent, "Format", pButton.texture, "formation shield")
end

-- BEASTMASTER --

tLeft.addButton("Beast", -0, 0, "ability_mount_swiftredwindrider", SynthiqBotsUI.tips.beast.master).doHide()
.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Beast"])
end

local tBeast = tLeft.addFrame("Beast", -2, 34)
tBeast:Hide()

tBeast.addButton("Release", 0, 0, "spell_nature_spiritwolf", SynthiqBotsUI.tips.beast.release)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTargetOrGroup("cast 2641")
end

tBeast.addButton("Revive", 0, 30, "ability_hunter_beastsoothe", SynthiqBotsUI.tips.beast.revive)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTargetOrGroup("cast 982")
end

tBeast.addButton("Heal", 0, 60, "ability_hunter_mendpet", SynthiqBotsUI.tips.beast.heal)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTargetOrGroup("cast 48990")
end

tBeast.addButton("Feed", 0, 90, "ability_hunter_beasttraining", SynthiqBotsUI.tips.beast.feed)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTargetOrGroup("cast 6991")
end

tBeast.addButton("Call", 0, 120, "ability_hunter_beastcall", SynthiqBotsUI.tips.beast.call)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTargetOrGroup("cast 883")
end

-- CREATOR --

tLeft.addButton("Creator", -0, 0, "inv_helmet_145a", SynthiqBotsUI.tips.creator.master).doHide()
.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Creator"])
	SynthiqBotsUI.frames["MultiBar"].frames["Units"]:Hide()
end

local tCreator = tLeft.addFrame("Creator", -2, 34)
tCreator:Hide()

tCreator.addButton("Warrior", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_warrior.blp", SynthiqBotsUI.tips.creator.warrior)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass warrior", "SAY")
end

tCreator.addButton("Warlock", 0, 30, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_warlock.blp", SynthiqBotsUI.tips.creator.warlock)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass warlock", "SAY")
end

tCreator.addButton("Shaman", 0, 60, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_shaman.blp", SynthiqBotsUI.tips.creator.shaman)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass shaman", "SAY")
end

tCreator.addButton("Rogue", 0, 90, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_rogue.blp", SynthiqBotsUI.tips.creator.rogue)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass rogue", "SAY")
end

tCreator.addButton("Priest", 0, 120, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_priest.blp", SynthiqBotsUI.tips.creator.priest)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass priest", "SAY")
end

tCreator.addButton("Paladin", 0, 150, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_paladin.blp", SynthiqBotsUI.tips.creator.paladin)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass paladin", "SAY")
end

tCreator.addButton("Mage", 0, 180, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_mage.blp", SynthiqBotsUI.tips.creator.mage)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass mage", "SAY")
end

tCreator.addButton("Hunter", 0, 210, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_hunter.blp", SynthiqBotsUI.tips.creator.hunter)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass hunter", "SAY")
end

tCreator.addButton("Druid", 0, 240, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_druid.blp", SynthiqBotsUI.tips.creator.druid)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass druid", "SAY")
end

tCreator.addButton("DeathKnight", 0, 270, "Interface\\AddOns\\synthiqbots-ui\\Icons\\addclass_deathknight.blp", SynthiqBotsUI.tips.creator.deathknight)
.doLeft = function(pButton)
	SendChatMessage(".playerbot bot addclass dk", "SAY")
end

tCreator.addButton("Inspect", 0, 300, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", SynthiqBotsUI.tips.creator.inspect)
.doLeft = function(pButton)
	local tName = UnitName("target")
	if(tName == nil or tName == "Unknown Entity") then return SendChatMessage("I dont have a Target.", "SAY") end
	InspectUnit(tName)
end

local tButton = tCreator.addButton("Init", 0, 330, "inv_misc_enggizmos_27", SynthiqBotsUI.tips.creator.init)
tButton.doRight = function(pButton)
	if(GetNumRaidMembers() > 0) then
		for i = 1, GetNumRaidMembers() do
			local tName = UnitName("raid" .. i)
			if(SynthiqBotsUI.isRoster("players", tName))	then SendChatMessage(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.player, "NAME", tName), "SAY")
			elseif(SynthiqBotsUI.isRoster("members", tName)) then SendChatMessage(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.member, "NAME", tName), "SAY")
			elseif(tName ~= UnitName("player")) then SendChatMessage(".playerbot bot init=auto " .. tName, "SAY")
			end
		end
		
		return
	end
	
	if(GetNumPartyMembers() > 0) then
		for i = 1, GetNumPartyMembers() do
			local tName = UnitName("party" .. i)
			if(SynthiqBotsUI.isRoster("players", tName))	then SendChatMessage(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.player, "NAME", tName), "SAY")
			elseif(SynthiqBotsUI.isRoster("members", tName)) then SendChatMessage(SynthiqBotsUI.doReplace(SynthiqBotsUI.info.member, "NAME", tName), "SAY")
			elseif(tName ~= UnitName("player")) then SendChatMessage(".playerbot bot init=auto " .. tName, "SAY")
			end
		end
		
		return
	end
	
	SendChatMessage(SynthiqBotsUI.info.group, "SAY")
end
tButton.doLeft = function(pButton)
	local tName = UnitName("target")
	if(tName == nil or tName == "Unknown Entity") then return SendChatMessage(SynthiqBotsUI.info.target, "SAY") end
	if(SynthiqBotsUI.isRoster("players", tName)) then return SendChatMessage(SynthiqBotsUI.info.players, "SAY") end
	if(SynthiqBotsUI.isRoster("members", tName)) then return SendChatMessage(SynthiqBotsUI.info.members, "SAY") end
	SendChatMessage(".playerbot bot init=auto " .. tName, "SAY")
end

-- UNITS --

local tButton = tMultiBar.addButton("Units", -38, 0, "inv_scroll_04", SynthiqBotsUI.tips.units.master)
tButton.roster = "players"
tButton.filter = "none"

tButton.doRight = function(pButton)
	-- MEMBERBOTS --
	
	for i = 1, 50 do
		local tName, tRank, tIndex, tLevel, tClass = GetGuildRosterInfo(i)
		
		-- Ensure that the Counter is not bigger than the Amount of Members in Guildlist
		if(tName ~= nil and tLevel ~= nil and tClass ~= nil and tName ~= UnitName("player")) then
			local tMember = SynthiqBotsUI.addMember(tClass, tLevel, tName)
			
			if(tMember.state == false)
			then tMember.setDisable()
			else tMember.setEnable()
			end
			
			tMember.doRight = function(pButton)
				if(pButton.state == false) then return end
				SendChatMessage(".playerbot bot remove " .. pButton.name, "SAY")
				if(pButton.parent.frames[pButton.name] ~= nil) then pButton.parent.frames[pButton.name]:Hide() end
				pButton.setDisable()
			end
			
			tMember.doLeft = function(pButton)
				if(pButton.state) then
					if(pButton.parent.frames[pButton.name] ~= nil) then SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames[pButton.name]) end
				else
					SendChatMessage(".playerbot bot add " .. pButton.name, "SAY")
					pButton.setEnable()
				end
			end
		else
			break
		end
	end
	
	-- FRIENDBOTS --
	
	for i = 1, 50 do
		local tName, tLevel, tClass = GetFriendInfo(i)
		
		-- Ensure that the Counter is not bigger than the Amount of Members in Friendlist
		if(tName ~= nil and tLevel ~= nil and tClass ~= nil and tName ~= UnitName("player")) then
			local tFriend = SynthiqBotsUI.addFriend(tClass, tLevel, tName)
			
			if(tFriend.state == false)
			then tFriend.setDisable()
			else tFriend.setEnable()
			end
			
			tFriend.doRight = function(pButton)
				if(pButton.state == false) then return end
				SendChatMessage(".playerbot bot remove " .. pButton.name, "SAY")
				if(pButton.parent.frames[pButton.name] ~= nil) then pButton.parent.frames[pButton.name]:Hide() end
				pButton.setDisable()
			end
			
			tFriend.doLeft = function(pButton)
				if(pButton.state) then
					if(pButton.parent.frames[pButton.name] ~= nil) then SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames[pButton.name]) end
				else
					SendChatMessage(".playerbot bot add " .. pButton.name, "SAY")
					pButton.setEnable()
				end
			end
		else
			break
		end
	end
	
	pButton.doLeft(pButton, pButton.roster, pButton.filter)
end

tButton.doLeft = function(pButton, oRoster, oFilter)
	local tUnits = pButton.parent.frames["Units"]
	local tTable = nil
	
	for key, value in pairs(tUnits.buttons) do value:Hide() end
	for key, value in pairs(tUnits.frames) do value:Hide() end
	tUnits.frames["Alliance"]:Show()
	tUnits.frames["Control"]:Show()
	
	if(oRoster == nil and oFilter == nil) then SynthiqBotsUI.ShowHideSwitch(tUnits)
	elseif(oRoster ~= nil) then pButton.roster = oRoster
	elseif(oFilter ~= nil) then pButton.filter = oFilter
	end
	
	if(pButton.filter ~= "none")
	then tTable = SynthiqBotsUI.index.classes[pButton.roster][pButton.filter]
	else tTable = SynthiqBotsUI.index[pButton.roster]
	end
	
	local tButton = nil
	local tFrame = nil
	local tIndex = 0
	
	if(tTable ~= nil)
	then pButton.limit = table.getn(tTable)
	else pButton.limit = 0
	end
	
	pButton.from = 1
	pButton.to = 10
	
	for i = 1, pButton.limit do
		tIndex = (i - 1)%10 + 1
		tFrame = tUnits.frames[tTable[i]]
		tButton = tUnits.buttons[tTable[i]]
		tButton.setPoint(0, (tUnits.size + 2) * (tIndex - 1))
		if(tFrame ~=nil) then tFrame.setPoint(-34, (tUnits.size + 2) * (tIndex - 1) + 2) end
		
		if(pButton.from <= i and pButton.to >= i) then
			if(tFrame ~= nil and tButton.state) then tFrame:Show() end
			tButton:Show()
		end
	end
	
	if(pButton.limit < pButton.to)
	then tUnits.frames["Control"].setPoint(-2, (tUnits.size + 2) * pButton.limit)
	else tUnits.frames["Control"].setPoint(-2, (tUnits.size + 2) * pButton.to)
	end
	
	if(pButton.limit < 11)
	then tUnits.frames["Control"].buttons["Browse"]:Hide()
	else tUnits.frames["Control"].buttons["Browse"]:Show()
	end
end

local tUnits = tMultiBar.addFrame("Units", -40, 72)
tUnits:Hide()

-- UNITS:ALLIANCE --

local tAlliance = tUnits.addFrame("Alliance", 0, -34, 32)
tAlliance:Show()

local tButton = tAlliance.addButton("Alliance", 0, 0, "inv_misc_tournaments_banner_human", SynthiqBotsUI.tips.units.alliance).doShow()
tButton.doRight = function(pButton)
	SendChatMessage(".playerbot bot remove *", "SAY");
end
tButton.doLeft = function(pButton)
	SendChatMessage(".playerbot bot add *", "SAY");
end

-- UNITS:CONTROL --

local tControl = tUnits.addFrame("Control", -2, 0)
tControl:Show()

-- UNITS:FILTER --

local tButton = tControl.addButton("Filter", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", SynthiqBotsUI.tips.units.filter)
tButton.doRight = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent, "Filter", "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp")
	tButton.doLeft(tButton, nil, "none")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Filter"])
end

local tFilter = tControl.addFrame("Filter", -30, 2)
tFilter:Hide()

tFilter.addButton("DeathKnight", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_deathknight.blp", SynthiqBotsUI.tips.units.deathknight)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "DeathKnight")
end

tFilter.addButton("Druid", -26, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_druid.blp", SynthiqBotsUI.tips.units.druid)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Druid")
end

tFilter.addButton("Hunter", -52, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_hunter.blp", SynthiqBotsUI.tips.units.hunter)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Hunter")
end

tFilter.addButton("Mage", -78, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_mage.blp", SynthiqBotsUI.tips.units.mage)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Mage")
end

tFilter.addButton("Paladin", -104, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_paladin.blp", SynthiqBotsUI.tips.units.paladin)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Paladin")
end

tFilter.addButton("Priest", -130, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_priest.blp", SynthiqBotsUI.tips.units.priest)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Priest")
end

tFilter.addButton("Rogue", -156, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_rogue.blp", SynthiqBotsUI.tips.units.rogue)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Rogue")
end

tFilter.addButton("Shaman", -182, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_shaman.blp", SynthiqBotsUI.tips.units.shaman)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Shaman")
end

tFilter.addButton("Warlock", -208, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_warlock.blp", SynthiqBotsUI.tips.units.warlock)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Warlock")
end

tFilter.addButton("Warrior", -234, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_warrior.blp", SynthiqBotsUI.tips.units.warrior)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "Warrior")
end

tFilter.addButton("None", -260, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", SynthiqBotsUI.tips.units.none)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Filter", pButton.texture)
	tButton.doLeft(tButton, nil, "none")
end

-- UNITS:ROSTER --

local tButton = tControl.addButton("Roster", 0, 30, "Interface\\AddOns\\synthiqbots-ui\\Icons\\roster_players.blp", SynthiqBotsUI.tips.units.roster)
tButton.doRight = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent, "Roster", "Interface\\AddOns\\synthiqbots-ui\\Icons\\roster_players.blp")
	tButton.doLeft(tButton, "players")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Roster"])
end

local tRoster = tControl.addFrame("Roster", -30, 32)
tRoster:Hide()

tRoster.addButton("Friends", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\roster_friends.blp", SynthiqBotsUI.tips.units.friends)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Roster", pButton.texture)
	pButton.parent.parent.buttons["Invite"].setEnable()
	pButton.parent.parent.frames["Invite"]:Hide()
	tButton.doLeft(tButton, "friends")
end

tRoster.addButton("Members", -26, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\roster_members.blp", SynthiqBotsUI.tips.units.members)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Roster", pButton.texture)
	pButton.parent.parent.buttons["Invite"].setEnable()
	pButton.parent.parent.frames["Invite"]:Hide()
	tButton.doLeft(tButton, "members")
end

tRoster.addButton("Players", -52, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\roster_players.blp", SynthiqBotsUI.tips.units.players)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Roster", pButton.texture)
	pButton.parent.parent.buttons["Invite"].setEnable()
	pButton.parent.parent.frames["Invite"]:Hide()
	tButton.doLeft(tButton, "players")
end

tRoster.addButton("Actives", -78, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\roster_actives.blp", SynthiqBotsUI.tips.units.actives)
.doLeft = function(pButton)
	local tButton = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	SynthiqBotsUI.Select(pButton.parent.parent, "Roster", pButton.texture)
	pButton.parent.parent.buttons["Invite"].setDisable()
	pButton.parent.parent.frames["Invite"]:Hide()
	tButton.doLeft(tButton, "actives")
end

-- UNITS:BROWSE --

local tButton = tControl.addButton("Invite", 0, 60, "Interface\\AddOns\\synthiqbots-ui\\Icons\\invite.blp", SynthiqBotsUI.tips.units.invite).setEnable()
tButton.doRight = function(pButton)
	if(GetNumRaidMembers() > 0 or GetNumPartyMembers() > 0) then
		return SendChatMessage(".playerbot bot remove *", "SAY")
	else
		SynthiqBotsUI.timer.invite.roster = SynthiqBotsUI.frames["MultiBar"].buttons["Units"].roster
		SynthiqBotsUI.timer.invite.needs = table.getn(SynthiqBotsUI.index[SynthiqBotsUI.timer.invite.roster])
		SynthiqBotsUI.timer.invite.index = 1
		SynthiqBotsUI.auto.invite = true
		SendChatMessage(SynthiqBotsUI.info.starting, "SAY")
	end
end
tButton.doLeft = function(pButton)
	if(pButton.state) then SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Invite"]) end
end

local tInvite = tControl.addFrame("Invite", -30, 62)
tInvite:Hide()

tInvite.addButton("Party+5", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\invite_party_5.blp", SynthiqBotsUI.tips.units.inviteParty5)
.doLeft = function(pButton)
	if(SynthiqBotsUI.auto.invite) then return SendChatMessage(SynthiqBotsUI.info.wait, "SAY") end
	local tRaid = GetNumRaidMembers()
	local tParty = GetNumPartyMembers()
	SynthiqBotsUI.timer.invite.roster = SynthiqBotsUI.frames["MultiBar"].buttons["Units"].roster
	SynthiqBotsUI.timer.invite.needs = SynthiqBotsUI.IF(tRaid > 0, 5 - tRaid, SynthiqBotsUI.IF(tParty > 0, 4 - tParty, 4))
	SynthiqBotsUI.timer.invite.index = 1
	SynthiqBotsUI.auto.invite = true
	pButton.parent:Hide()
	SendChatMessage(SynthiqBotsUI.info.starting, "SAY")
end

tInvite.addButton("Raid+10", 56, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\invite_raid_10.blp", SynthiqBotsUI.tips.units.inviteRaid10)
.doLeft = function(pButton)
	if(SynthiqBotsUI.auto.invite) then return SendChatMessage(SynthiqBotsUI.info.wait, "SAY") end
	local tRaid = GetNumRaidMembers()
	local tParty = GetNumPartyMembers()
	SynthiqBotsUI.timer.invite.roster = SynthiqBotsUI.frames["MultiBar"].buttons["Units"].roster
	SynthiqBotsUI.timer.invite.needs = 10 - SynthiqBotsUI.IF(tRaid > 0, tRaid, SynthiqBotsUI.IF(tParty > 0, tParty + 1, 1))
	SynthiqBotsUI.timer.invite.index = 1
	SynthiqBotsUI.auto.invite = true
	pButton.parent:Hide()
	SendChatMessage(SynthiqBotsUI.info.starting, "SAY")
end

tInvite.addButton("Raid+25", 82, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\invite_raid_25.blp", SynthiqBotsUI.tips.units.inviteRaid25)
.doLeft = function(pButton)
	if(SynthiqBotsUI.auto.invite) then return SendChatMessage(SynthiqBotsUI.info.wait, "SAY") end
	local tRaid = GetNumRaidMembers()
	local tParty = GetNumPartyMembers()
	SynthiqBotsUI.timer.invite.roster = SynthiqBotsUI.frames["MultiBar"].buttons["Units"].roster
	SynthiqBotsUI.timer.invite.needs = 25 - SynthiqBotsUI.IF(tRaid > 0, tRaid, SynthiqBotsUI.IF(tParty > 0, tParty + 1, 1))
	SynthiqBotsUI.timer.invite.index = 1
	SynthiqBotsUI.auto.invite = true
	pButton.parent:Hide()
	SendChatMessage(SynthiqBotsUI.info.starting, "SAY")
end

tInvite.addButton("Raid+40", 108, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\invite_raid_40.blp", SynthiqBotsUI.tips.units.inviteRaid40)
.doLeft = function(pButton)
	if(SynthiqBotsUI.auto.invite) then return SendChatMessage(SynthiqBotsUI.info.wait, "SAY") end
	local tRaid = GetNumRaidMembers()
	local tParty = GetNumPartyMembers()
	SynthiqBotsUI.timer.invite.roster = SynthiqBotsUI.frames["MultiBar"].buttons["Units"].roster
	SynthiqBotsUI.timer.invite.needs = 40 - SynthiqBotsUI.IF(tRaid > 0, tRaid, SynthiqBotsUI.IF(tParty > 0, tParty + 1, 1))
	SynthiqBotsUI.timer.invite.index = 1
	SynthiqBotsUI.auto.invite = true
	pButton.parent:Hide()
	SendChatMessage(SynthiqBotsUI.info.starting, "SAY")
end

tControl.addButton("Browse", 0, 90, "Interface\\AddOns\\synthiqbots-ui\\Icons\\browse.blp", SynthiqBotsUI.tips.units.browse)
.doLeft = function(pButton)
	local tMaster = SynthiqBotsUI.frames["MultiBar"].buttons["Units"]
	local tFrom = tMaster.from + 10
	local tTo = tMaster.to + 10
	
	if(tMaster.filter ~= "none")
	then tTable = SynthiqBotsUI.index.classes[tMaster.roster][tMaster.filter]
	else tTable = SynthiqBotsUI.index[tMaster.roster]
	end
	
	local tUnits = tMaster.parent.frames["Units"]
	local tButton = nil
	local tFrame = nil
	local tIndex = 0
	
	if(tFrom > tMaster.limit) then
		tFrom = 1
		tTo = 10
	end
	
	if(tTo > tMaster.limit) then
		tTo = tMaster.limit
	end
	
	for i = 1, tMaster.limit do
		tFrame = tUnits.frames[tTable[i]]
		tButton = tUnits.buttons[tTable[i]]
		
		if(tMaster.from <= i and tMaster.to >= i) then
			if(tFrame ~= nil) then tFrame:Hide() end
			tButton:Hide()
		end
		
		if(tFrom <= i and tTo >= i) then
			tIndex = tIndex + 1
			if(tFrame ~= nil and tButton.state) then tFrame:Show() end 
			tButton:Show()
		end
	end
	
	tMaster.from = tFrom
	tMaster.to = tTo
	
	tUnits.frames["Control"].setPoint(-2, (tUnits.size + 2) * tIndex)
end

-- MAIN --

local tButton = tMultiBar.addButton("Main", 0, 0, "inv_gizmo_02", SynthiqBotsUI.tips.main.master)
tButton:RegisterForDrag("RightButton")
tButton:SetScript("OnDragStart", function()
	SynthiqBotsUI.frames["MultiBar"]:StartMoving()
end)
tButton:SetScript("OnDragStop", function()
	SynthiqBotsUI.frames["MultiBar"]:StopMovingOrSizing()
end)
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Main"])
end

local tMain = tMultiBar.addFrame("Main", -2, 38)
tMain:Hide()

tMain.addButton("Coords", 0, 0, "inv_gizmo_03", SynthiqBotsUI.tips.main.coords)
.doLeft = function(pButton)
	SynthiqBotsUI.frames["MultiBar"].setPoint(-262, 144)
	SynthiqBotsUI.inventory.setPoint(-700, -144)
	SynthiqBotsUI.spellbook.setPoint(-802, 302)
	SynthiqBotsUI.talent.setPoint(-104, -276)
	SynthiqBotsUI.reward.setPoint(-754,  238)
	SynthiqBotsUI.itemus.setPoint(-860, -144)
	SynthiqBotsUI.iconos.setPoint(-860, -144)
	SynthiqBotsUI.stats.setPoint(-60, 560)
end

tMain.addButton("Masters", 0, 34, "mail_gmicon", SynthiqBotsUI.tips.main.masters).setDisable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.GM == false) then return SendChatMessage(SynthiqBotsUI.info.rights, "SAY") end
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.doRepos("Right", 38)
		SynthiqBotsUI.frames["MultiBar"].frames["Masters"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].buttons["Masters"]:Show()
	else
		SynthiqBotsUI.doRepos("Right", -38)
		SynthiqBotsUI.frames["MultiBar"].frames["Masters"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].buttons["Masters"]:Hide()
	end
end

tMain.addButton("RTSC", 0, 68, "ability_hunter_markedfordeath", SynthiqBotsUI.tips.main.rtsc).setDisable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.frames["MultiBar"].setPoint(SynthiqBotsUI.frames["MultiBar"].x, SynthiqBotsUI.frames["MultiBar"].y + 34)
		SynthiqBotsUI.frames["MultiBar"].frames["RTSC"]:Show()
		SynthiqBotsUI.ActionToGroup("rtsc")
	else
		SynthiqBotsUI.frames["MultiBar"].setPoint(SynthiqBotsUI.frames["MultiBar"].x, SynthiqBotsUI.frames["MultiBar"].y - 34)
		SynthiqBotsUI.frames["MultiBar"].frames["RTSC"]:Hide()
		SynthiqBotsUI.ActionToGroup("rtsc reset")
	end
end

tMain.addButton("Raidus", 0, 102, "inv_misc_head_dragon_01", SynthiqBotsUI.tips.main.raidus).setDisable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.raidus.setRaidus()
		SynthiqBotsUI.raidus:Show()
	else
		SynthiqBotsUI.raidus:Hide()
	end
end

tMain.addButton("Creator", 0, 136, "inv_helmet_145a", SynthiqBotsUI.tips.main.creator).setDisable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.doRepos("Tanker", -34)
		SynthiqBotsUI.doRepos("Attack", -34)
		SynthiqBotsUI.doRepos("Mode", -34)
		SynthiqBotsUI.doRepos("Stay", -34)
		SynthiqBotsUI.doRepos("Follow", -34)
		SynthiqBotsUI.doRepos("ExpandStay", -34)
		SynthiqBotsUI.doRepos("ExpandFollow", -34)
		SynthiqBotsUI.doRepos("Flee", -34)
		SynthiqBotsUI.doRepos("Format", -34)
		SynthiqBotsUI.doRepos("Beast", -34)
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Creator"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Creator"]:Show()
	else
		SynthiqBotsUI.doRepos("Tanker", 34)
		SynthiqBotsUI.doRepos("Attack", 34)
		SynthiqBotsUI.doRepos("Mode", 34)
		SynthiqBotsUI.doRepos("Stay", 34)
		SynthiqBotsUI.doRepos("Follow", 34)
		SynthiqBotsUI.doRepos("ExpandStay", 34)
		SynthiqBotsUI.doRepos("ExpandFollow", 34)
		SynthiqBotsUI.doRepos("Flee", 34)
		SynthiqBotsUI.doRepos("Format", 34)
		SynthiqBotsUI.doRepos("Beast", 34)
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Creator"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Creator"]:Hide()
	end
end

tMain.addButton("Beast", 0, 170, "ability_mount_swiftredwindrider", SynthiqBotsUI.tips.main.beast).setDisable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.doRepos("Tanker", -34)
		SynthiqBotsUI.doRepos("Attack", -34)
		SynthiqBotsUI.doRepos("Mode", -34)
		SynthiqBotsUI.doRepos("Stay", -34)
		SynthiqBotsUI.doRepos("Follow", -34)
		SynthiqBotsUI.doRepos("ExpandStay", -34)
		SynthiqBotsUI.doRepos("ExpandFollow", -34)
		SynthiqBotsUI.doRepos("Flee", -34)
		SynthiqBotsUI.doRepos("Format", -34)
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Beast"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Beast"]:Show()
	else
		SynthiqBotsUI.doRepos("Tanker", 34)
		SynthiqBotsUI.doRepos("Attack", 34)
		SynthiqBotsUI.doRepos("Mode", 34)
		SynthiqBotsUI.doRepos("Stay", 34)
		SynthiqBotsUI.doRepos("Follow", 34)
		SynthiqBotsUI.doRepos("ExpandStay", 34)
		SynthiqBotsUI.doRepos("ExpandFollow", 34)
		SynthiqBotsUI.doRepos("Flee", 34)
		SynthiqBotsUI.doRepos("Format", 34)
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].frames["Beast"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Beast"]:Hide()
	end
end

--[[
local tButton = tMain.addButton("Language", 0, 34, "Interface\\AddOns\\synthiqbots-ui\\Icons\\language_none.blp", SynthiqBotsUI.tips.main.lang.master).setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.auto.language = SynthiqBotsUI.OnOffSwitch(pButton) == false
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.auto.language == true) then return SendChatMessage(SynthiqBotsUI.info.language, "SAY") end
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Language"])
end

local tFrame = tMain.addFrame("Language", -36, 36)
tFrame:Hide()

tFrame.addButton("deDE", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\language_deDE.blp", SynthiqBotsUI.tips.main.lang.deDE)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(pButton.parent.parent, "Language", pButton.texture)
	SynthiqBotsUI.doSlash("/reload")
	SynthiqBotsUI.language = "deDE"
end

tFrame.addButton("enGB", -30, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\language_enGB.blp", SynthiqBotsUI.tips.main.lang.enGB)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(pButton.parent.parent, "Language", pButton.texture)
	SynthiqBotsUI.doSlash("/reload")
	SynthiqBotsUI.language = "enGB"
end

tFrame.addButton("None", -60, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\language_none.blp", SynthiqBotsUI.tips.main.lang.none)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(pButton.parent.parent, "Language", pButton.texture)
	SynthiqBotsUI.doSlash("/reload")
	SynthiqBotsUI.language = "none"
end
]]--

tMain.addButton("Expand", 0, 204, "Interface\\AddOns\\synthiqbots-ui\\Icons\\command_follow.blp", SynthiqBotsUI.tips.main.expand).setDisable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.doRepos("Tanker", -34)
		SynthiqBotsUI.doRepos("Attack", -34)
		SynthiqBotsUI.doRepos("Mode", -34)
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["ExpandFollow"]:Show()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["ExpandStay"]:Show()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Follow"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Stay"]:Hide()
	else
		SynthiqBotsUI.doRepos("Tanker", 34)
		SynthiqBotsUI.doRepos("Attack", 34)
		SynthiqBotsUI.doRepos("Mode", 34)
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["ExpandFollow"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["ExpandStay"]:Hide()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Follow"]:Show()
		SynthiqBotsUI.frames["MultiBar"].frames["Left"].buttons["Stay"]:Show()
	end
end

tMain.addButton("Release", 0, 238, "achievement_bg_xkills_avgraveyard", SynthiqBotsUI.tips.main.release).setDisable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.auto.release = true
	else
		SynthiqBotsUI.auto.release = false
	end
end

tMain.addButton("Stats", 0, 272, "inv_scroll_08", SynthiqBotsUI.tips.main.stats).setDisable()
.doLeft = function(pButton)
	if(GetNumRaidMembers() > 0) then return SendChatMessage(SynthiqBotsUI.info.stats, "SAY") end
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.auto.stats = true
		for i = 1, GetNumPartyMembers() do SendChatMessage("stats", "WHISPER", nil, UnitName("party" .. i)) end
		SynthiqBotsUI.stats:Show()
	else
		SynthiqBotsUI.auto.stats = false
		for key, value in pairs(SynthiqBotsUI.stats.frames) do value:Hide() end
		SynthiqBotsUI.stats:Hide()
	end
end

local tButton = tMain.addButton("Reward", 0, 306, "Interface\\AddOns\\synthiqbots-ui\\Icons\\reward.blp", SynthiqBotsUI.tips.main.reward).setDisable()
tButton.doRight = function(pButton)
	if(table.getn(SynthiqBotsUI.reward.rewards) > 0 and table.getn(SynthiqBotsUI.reward.units) > 0) then SynthiqBotsUI.reward:Show() end
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.reward.state = SynthiqBotsUI.OnOffSwitch(pButton)
end

tMain.addButton("Reset", 0, 340, "inv_misc_tournaments_symbol_gnome", SynthiqBotsUI.tips.main.reset)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTargetOrGroup("reset botAI")
end

tMain.addButton("Actions", 0, 374, "inv_helmet_02", SynthiqBotsUI.tips.main.action)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToTargetOrGroup("reset")
end

tMain.addButton("AutoChat", 0, 408, "spell_holy_silence", SynthiqBotsUI.tips.main.autochat).setEnable()
.doLeft = function(pButton)
	if(SynthiqBotsUI.OnOffSwitch(pButton)) then
		SynthiqBotsUI.auto.botCommands = true
	else
		SynthiqBotsUI.auto.botCommands = false
	end
end

tMain.addButton("Feedback", 0, 442, "inv_misc_note_01", SynthiqBotsUI.tips.main.feedback)
.doLeft = function(pButton)
	if(SynthiqBotsUI.feedback ~= nil and SynthiqBotsUI.feedback.capture ~= nil) then
		SynthiqBotsUI.feedback.capture("")
	end
end

-- GAMEMASTER --

local tButton = tMultiBar.addButton("Masters", 38, 0, "mail_gmicon", SynthiqBotsUI.tips.game.master).doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.doSlash("/SynthiqBotsUI", "")
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Masters"])
end

local tMasters = tMultiBar.addFrame("Masters", 36, 38)
tMasters:Hide()

tMasters.addButton("NecroNet", 0, 0, "achievement_bg_xkills_avgraveyard", SynthiqBotsUI.tips.game.necronet).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		SynthiqBotsUI.necronet.state = false
		for key, value in pairs(SynthiqBotsUI.necronet.buttons) do value:Hide() end
		pButton.setDisable()
	else
		SynthiqBotsUI.necronet.cont = 0
		SynthiqBotsUI.necronet.area = 0
		SynthiqBotsUI.necronet.zone = 0
		SynthiqBotsUI.necronet.state = true
		pButton.setEnable()
	end
end

tMasters.addButton("Portal", 0, 34, "inv_box_02", SynthiqBotsUI.tips.game.portal)
.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Portal"])
end

local tPortal = tMasters.addFrame("Portal", 30, 36)
tPortal:Hide()

local tButton = tPortal.addButton("Red", 0, 0, "inv_jewelcrafting_gem_16", SynthiqBotsUI.doReplace(SynthiqBotsUI.tips.game.memory, "ABOUT", SynthiqBotsUI.info.location)).setDisable()
tButton.goMap = ""
tButton.goX = 0
tButton.goY = 0
tButton.goZ = 0

tButton.doRight = function(pButton)
	if(pButton.state == false) then return SendChatMessage(SynthiqBotsUI.info.itlocation, "SAY") end
	pButton.tip = SynthiqBotsUI.doReplace(SynthiqBotsUI.tips.game.memory, "ABOUT", SynthiqBotsUI.info.location)
	pButton.setDisable()
end
tButton.doLeft = function(pButton)
	local tPlayer = SynthiqBotsUI.getBot(UnitName("player"))
	if(tPlayer.waitFor == nil) then tPlayer.waitFor = "" end
	if(tPlayer.waitFor ~= "") then return SendChatMessage(SynthiqBotsUI.info.saving, "SAY") end
	if(pButton.state) then return SendChatMessage(".go xyz " .. pButton.goX .. " " .. pButton.goY .. " " .. pButton.goZ .. " " .. pButton.goMap, "SAY")	end
	tPlayer.memory = pButton
	tPlayer.waitFor = "COORDS"
	SendChatMessage(".gps", "SAY")
end

local tButton = tPortal.addButton("Green", 30, 0, "inv_jewelcrafting_gem_13", SynthiqBotsUI.doReplace(SynthiqBotsUI.tips.game.memory, "ABOUT", SynthiqBotsUI.info.location)).setDisable()
tButton.goMap = ""
tButton.goX = 0
tButton.goY = 0
tButton.goZ = 0

tButton.doRight = function(pButton)
	if(pButton.state == false) then return SendChatMessage(SynthiqBotsUI.info.itlocation, "SAY") end
	pButton.tip = SynthiqBotsUI.doReplace(SynthiqBotsUI.tips.game.memory, "ABOUT", SynthiqBotsUI.info.location)
	pButton.setDisable()
end
tButton.doLeft = function(pButton)
	local tPlayer = SynthiqBotsUI.getBot(UnitName("player"))
	if(tPlayer.waitFor == nil) then tPlayer.waitFor = "" end
	if(tPlayer.waitFor ~= "") then return SendChatMessage(SynthiqBotsUI.info.saving, "SAY") end
	if(pButton.state) then return SendChatMessage(".go xyz " .. pButton.goX .. " " .. pButton.goY .. " " .. pButton.goZ .. " " .. pButton.goMap, "SAY")	end
	tPlayer.memory = pButton
	tPlayer.waitFor = "COORDS"
	SendChatMessage(".gps", "SAY")
end

local tButton = tPortal.addButton("Blue", 60, 0, "inv_jewelcrafting_gem_17", SynthiqBotsUI.doReplace(SynthiqBotsUI.tips.game.memory, "ABOUT", SynthiqBotsUI.info.location)).setDisable()
tButton.goMap = ""
tButton.goX = 0
tButton.goY = 0
tButton.goZ = 0

tButton.doRight = function(pButton)
	if(pButton.state == false) then return SendChatMessage(SynthiqBotsUI.info.itlocation, "SAY") end
	pButton.tip = SynthiqBotsUI.doReplace(SynthiqBotsUI.tips.game.memory, "ABOUT", SynthiqBotsUI.info.location)
	pButton.setDisable()
end
tButton.doLeft = function(pButton)
	local tPlayer = SynthiqBotsUI.getBot(UnitName("player"))
	if(tPlayer.waitFor == nil) then tPlayer.waitFor = "" end
	if(tPlayer.waitFor ~= "") then return SendChatMessage(SynthiqBotsUI.info.saving, "SAY") end
	if(pButton.state) then return SendChatMessage(".go xyz " .. pButton.goX .. " " .. pButton.goY .. " " .. pButton.goZ .. " " .. pButton.goMap, "SAY")	end
	tPlayer.memory = pButton
	tPlayer.waitFor = "COORDS"
	SendChatMessage(".gps", "SAY")
end

tMasters.addButton("Itemus", 0, 68, "inv_box_01", SynthiqBotsUI.tips.game.itemus)
.doLeft = function(pButton)
	if(SynthiqBotsUI.ShowHideSwitch(SynthiqBotsUI.itemus)) then
		SynthiqBotsUI.itemus.addItems()
	end
end

tMasters.addButton("Iconos", 0, 102, "inv_mask_01", SynthiqBotsUI.tips.game.iconos)
.doLeft = function(pButton)
	if(SynthiqBotsUI.ShowHideSwitch(SynthiqBotsUI.iconos)) then
		SynthiqBotsUI.iconos.addIcons()
	end
end

tMasters.addButton("Summon", 0, 136, "spell_holy_prayerofspirit", SynthiqBotsUI.tips.game.summon)
.doLeft = function(pButton)
	SynthiqBotsUI.doDotWithTarget(".summon")
end

tMasters.addButton("Appear", 0, 170, "spell_holy_divinespirit", SynthiqBotsUI.tips.game.appear)
.doLeft = function(pButton)
	SynthiqBotsUI.doDotWithTarget(".appear")
end

-- RIGHT --

local tRight = tMultiBar.addFrame("Right", 34, 2, 32)

-- QUEST --

local tButton = tRight.addButton("Quests", 0, 0, "inv_misc_book_07", SynthiqBotsUI.tips.quests.master)
tButton.doRight = function(pButton)
	local tEntries, tQuests = GetNumQuestLogEntries()
	local tFrame = pButton.parent.frames["Quests"]
	local tIndex = 0
	local tShow = 1
	
	for key, value in pairs(tFrame.buttons) do value:Hide() end
	for key, value in pairs(tFrame.texts) do value:Hide() end
	table.wipe(tFrame.buttons)
	table.wipe(tFrame.texts)
	tFrame.buttons = {}
	tFrame.texts = {}
	
	tFrame.limit = 0
	tFrame.from = 1
	tFrame.to = 10
	
	tFrame.addButton("Browse", 0, 300, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_browse.blp", "")
	.doLeft = function(pButton)
		local tFrom = pButton.parent.from + 10
		local tTo = pButton.parent.to + 10
		
		if(tFrom > pButton.parent.limit) then
			tFrom = 1
			tTo = 10
		end
		
		if(tTo > pButton.parent.limit) then
			tTo = pButton.parent.limit
		end
		
		for i = 1, pButton.parent.limit do
			if(i < tFrom or i > tTo) then
				pButton.parent.buttons["Quest" .. i]:Hide()
				pButton.parent.texts["Title" .. i]:Hide()
			else
				pButton.parent.buttons["Quest" .. i]:Show()
				pButton.parent.texts["Title" .. i]:Show()
			end
		end
		
		tFrame.from = tFrom
		tFrame.to = tTo
		
		pButton.setPoint(0, ((tTo - 1)%10 + 1) * 30)
	end
	
	for i = 1, tEntries do
		local tLink = GetQuestLink(i)
		local tTitle, tLevel, tGroup, tHeader, tCollapsed, tComplete = GetQuestLogTitle(i)
		
		if(tCollapsed == nil) then
			tFrame.limit = tFrame.limit + 1
			
			local tAmount = 0
			local tButton = tFrame.addButton("Quest" .. tFrame.limit, 0, tIndex * 30, "inv_misc_note_01", tLink)
			tButton.link = tLink
			tButton.id = i
			
			tButton.doRight = function(pButton)
				if(GetNumRaidMembers() > 0) then
					SendChatMessage("drop " .. pButton.link, "RAID")
				elseif(GetNumPartyMembers() > 0) then
					SendChatMessage("drop " .. pButton.link, "PARTY")
				end
				
				SelectQuestLogEntry(pButton.id)
				SetAbandonQuest()
				AbandonQuest()
			end
			
			tButton.doLeft = function(pButton)
				SelectQuestLogEntry(pButton.id)
				QuestLogPushQuest()
			end
			
			if(GetNumRaidMembers() > 0) then
				for n = 1, 40 do
					if(IsUnitOnQuest(i, "raid" .. n)) then tAmount = tAmount + 1 end
				end
			elseif(GetNumPartyMembers() > 0) then
				for n = 1, 4 do
					if(IsUnitOnQuest(i, "party" .. n)) then tAmount = tAmount + 1 end
				end
			end
			
			local tText = tFrame.addText("Title" .. tFrame.limit, "[" .. tAmount .. "] " .. tTitle, "BOTTOMLEFT", 30, tIndex * 30 + 14, 12)
			
			if(tShow >= tFrame.from and tShow <= tFrame.to) then
				tButton:Show()
				tText:Show()
			else
				tButton:Hide()
				tText:Hide()
			end
			
			tShow = tShow + 1
			tIndex = (tIndex + 1)%10
		end
	end
	
	if(tFrame.limit > 10)
	then tFrame.buttons["Browse"]:Show()
	else tFrame.buttons["Browse"]:Hide()
	end
end
tButton.doLeft = function(pButton)
	if(SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Quests"])) then pButton.doRight(pButton) end
end

local tQuests = tRight.addFrame("Quests", -2, 64)
tQuests:Hide()

local tAccpet = tQuests.addFrame("Accept", 0, -30, 28)
tAccpet:Show()

tAccpet.addButton("Accept", 0, 0, "inv_misc_note_02", SynthiqBotsUI.tips.quests.accept)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("accept *")
end

-- DRINK --

tRight.addButton("Drink", 34, 0, "inv_drink_24_sealwhey", SynthiqBotsUI.tips.drink.group)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("drink")
end

-- RELEASE --

tRight.addButton("Release", 68, 0, "achievement_bg_xkills_avgraveyard", SynthiqBotsUI.tips.release.group)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("release")
end

-- REVIVE --

tRight.addButton("Revive", 102, 0, "spell_holy_guardianspirit", SynthiqBotsUI.tips.revive.group)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("revive")
end

-- SUMALL --

tRight.addButton("Summon", 136, 0, "ability_hunter_beastcall", SynthiqBotsUI.tips.summon.group)
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("summon")
end

-- INVENTORY --

SynthiqBotsUI.inventory = SynthiqBotsUI.newFrame(SynthiqBotsUI, -700, -144, 32, 442, 884)
SynthiqBotsUI.inventory.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Inventory.blp")
SynthiqBotsUI.inventory.addText("Title", "Inventory", "CENTER", -58, 429, 12)
SynthiqBotsUI.inventory.action = "s"
SynthiqBotsUI.inventory:SetMovable(true)
SynthiqBotsUI.inventory:Hide()

SynthiqBotsUI.inventory.movButton("Move", -406, 849, 34, SynthiqBotsUI.tips.move.inventory)

SynthiqBotsUI.inventory.wowButton("X", -126, 862, 15, 18, 13)
.doLeft = function(pButton)
	local tUnits = SynthiqBotsUI.frames["MultiBar"].frames["Units"]
	local tButton = tUnits.frames[SynthiqBotsUI.inventory.name].buttons["Inventory"]
	tButton.doLeft(tButton)
end

SynthiqBotsUI.inventory.addButton("Sell", -94, 806, "inv_misc_coin_16", SynthiqBotsUI.tips.inventory.sell).setEnable()
.doLeft = function(pButton)
	if(pButton.state) then
		SynthiqBotsUI.inventory.action = ""
		pButton.setDisable()
	else
		CancelTrade()
		SynthiqBotsUI.inventory.action = "s"
		pButton.getButton("Destroy").setDisable()
		pButton.getButton("Equip").setDisable()
		pButton.getButton("Trade").setDisable()
		pButton.getButton("Use").setDisable()
		pButton.setEnable()
	end
end

SynthiqBotsUI.inventory.addButton("Equip", -94, 768, "inv_helmet_22", SynthiqBotsUI.tips.inventory.equip).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		SynthiqBotsUI.inventory.action = ""
		pButton.setDisable()
	else
		CancelTrade()
		SynthiqBotsUI.inventory.action = "e"
		pButton.getButton("Destroy").setDisable()
		pButton.getButton("Trade").setDisable()
		pButton.getButton("Sell").setDisable()
		pButton.getButton("Use").setDisable()
		pButton.setEnable()
	end
end

SynthiqBotsUI.inventory.addButton("Use", -94, 731, "inv_gauntlets_25", SynthiqBotsUI.tips.inventory.use).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		SynthiqBotsUI.inventory.action = ""
		pButton.setDisable()
	else
		CancelTrade()
		SynthiqBotsUI.inventory.action = "u"
		pButton.getButton("Destroy").setDisable()
		pButton.getButton("Equip").setDisable()
		pButton.getButton("Trade").setDisable()
		pButton.getButton("Sell").setDisable()
		pButton.setEnable()
	end
end

SynthiqBotsUI.inventory.addButton("Trade", -94, 694, "achievement_reputation_01", SynthiqBotsUI.tips.inventory.trade).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		SynthiqBotsUI.inventory.action = ""
		pButton.setDisable()
		CancelTrade()
	else
		InitiateTrade(pButton.getName())
		SynthiqBotsUI.inventory.action = "give"
		pButton.getButton("Destroy").setDisable()
		pButton.getButton("Equip").setDisable()
		pButton.getButton("Sell").setDisable()
		pButton.getButton("Use").setDisable()
		pButton.setEnable()
	end
end

SynthiqBotsUI.inventory.addButton("Destroy", -94, 657, "inv_hammer_15", SynthiqBotsUI.tips.inventory.drop).setDisable()
.doLeft = function(pButton)
	if(pButton.state) then
		SynthiqBotsUI.inventory.action = ""
		pButton.setDisable()
	else
		CancelTrade()
		SynthiqBotsUI.inventory.action = "destroy"
		pButton.getButton("Equip").setDisable()
		pButton.getButton("Trade").setDisable()
		pButton.getButton("Sell").setDisable()
		pButton.getButton("Use").setDisable()
		pButton.setEnable()
	end
end

SynthiqBotsUI.inventory.addButton("Open", -94, 322.5, "inv_misc_gift_05", SynthiqBotsUI.tips.inventory.open)
.doLeft = function(pButton)
	SendChatMessage("open items", "WHISPER", nil, pButton.getName())
end

local tFrame = SynthiqBotsUI.inventory.addFrame("Items", -397, 807, 32)
tFrame:Show()

-- STATS --

SynthiqBotsUI.stats = SynthiqBotsUI.newFrame(SynthiqBotsUI, -60, 560, 32)
SynthiqBotsUI.stats:SetMovable(true)
SynthiqBotsUI.stats:Hide()

SynthiqBotsUI.stats.movButton("Move", 0, -80, 160, SynthiqBotsUI.tips.move.stats)

SynthiqBotsUI.addStats(SynthiqBotsUI.stats, "party1", 0,    0, 32, 192, 96)
SynthiqBotsUI.addStats(SynthiqBotsUI.stats, "party2", 0,  -60, 32, 192, 96)
SynthiqBotsUI.addStats(SynthiqBotsUI.stats, "party3", 0, -120, 32, 192, 96)
SynthiqBotsUI.addStats(SynthiqBotsUI.stats, "party4", 0, -180, 32, 192, 96)

-- ITEMUS --

SynthiqBotsUI.itemus = SynthiqBotsUI.newFrame(SynthiqBotsUI, -860, -144, 32, 442, 884)
SynthiqBotsUI.itemus.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Inventory.blp")
SynthiqBotsUI.itemus.addText("Title", "Itemus", "CENTER", -57, 429, 13)
SynthiqBotsUI.itemus.addText("Pages", "0/0", "CENTER", -57, 409, 13)
SynthiqBotsUI.itemus.name = UnitName("Player")
SynthiqBotsUI.itemus.index = {}
SynthiqBotsUI.itemus.color = "cff9d9d9d"
SynthiqBotsUI.itemus.level = "L10"
SynthiqBotsUI.itemus.rare = "R00"
SynthiqBotsUI.itemus.slot = "S00"
SynthiqBotsUI.itemus.type = "PC"
SynthiqBotsUI.itemus.max = 1
SynthiqBotsUI.itemus.now = 1
SynthiqBotsUI.itemus:SetMovable(true)
SynthiqBotsUI.itemus:Hide()

SynthiqBotsUI.itemus.movButton("Move", -407, 850, 32, SynthiqBotsUI.tips.move.itemus)

SynthiqBotsUI.itemus.wowButton("<", -319, 841, 15, 18, 13).doHide()
.doLeft = function(pButton)
	SynthiqBotsUI.itemus.now = SynthiqBotsUI.itemus.now - 1
	SynthiqBotsUI.itemus.addItems()
end

SynthiqBotsUI.itemus.wowButton(">", -225, 841, 15, 18, 13).doHide()
.doLeft = function(pButton)
	SynthiqBotsUI.itemus.now = SynthiqBotsUI.itemus.now + 1
	SynthiqBotsUI.itemus.addItems()
end

SynthiqBotsUI.itemus.wowButton("X", -126, 862, 15, 18, 13)
.doLeft = function(pButton)
	SynthiqBotsUI.itemus:Hide()
end

local tFrame = SynthiqBotsUI.itemus.addFrame("Items", -397, 807, 32)
tFrame:Show()

-- ITEMUS:LEVEL --

SynthiqBotsUI.itemus.addButton("Level", -94, 806, "achievement_level_10", SynthiqBotsUI.tips.itemus.level.master).setEnable()
.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Level"])
end

local tFrame = SynthiqBotsUI.itemus.addFrame("Level", -61, 808, 28)
tFrame:Hide()

tFrame.addButton("L10", 0, 0, "achievement_level_10", SynthiqBotsUI.tips.itemus.level.L10)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L10"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("L20", 30, 0, "achievement_level_20", SynthiqBotsUI.tips.itemus.level.L20)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L20"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("L30", 60, 0, "achievement_level_30", SynthiqBotsUI.tips.itemus.level.L30)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L30"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("L40", 90, 0, "achievement_level_40", SynthiqBotsUI.tips.itemus.level.L40)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L40"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("L50", 120, 0, "achievement_level_50", SynthiqBotsUI.tips.itemus.level.L50)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L50"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("L60", 150, 0, "achievement_level_60", SynthiqBotsUI.tips.itemus.level.L60)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L60"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("L70", 180, 0, "achievement_level_70", SynthiqBotsUI.tips.itemus.level.L70)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L70"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("L80", 210, 0, "achievement_level_80", SynthiqBotsUI.tips.itemus.level.L80)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Level", pButton.texture)
	SynthiqBotsUI.itemus.level = "L80"
	SynthiqBotsUI.itemus.addItems(1)
end

-- ITEMUS:RARE --

SynthiqBotsUI.itemus.addButton("Rare", -94, 768, "achievement_quests_completed_01", SynthiqBotsUI.tips.itemus.rare.master)
.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Rare"])
end

local tFrame = SynthiqBotsUI.itemus.addFrame("Rare", -61, 770)
tFrame:Hide()

tFrame.addButton("R00", 0, 0, "achievement_quests_completed_01", SynthiqBotsUI.tips.itemus.rare.R00)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cff9d9d9d"
	SynthiqBotsUI.itemus.rare = "R00"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("R01", 30, 0, "achievement_quests_completed_02", SynthiqBotsUI.tips.itemus.rare.R01)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cffffffff"
	SynthiqBotsUI.itemus.rare = "R01"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("R02", 60, 0, "achievement_quests_completed_03", SynthiqBotsUI.tips.itemus.rare.R02)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cff1eff00"
	SynthiqBotsUI.itemus.rare = "R02"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("R03", 90, 0, "achievement_quests_completed_04", SynthiqBotsUI.tips.itemus.rare.R03)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cff0070dd"
	SynthiqBotsUI.itemus.rare = "R03"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("R04", 120, 0, "achievement_quests_completed_05", SynthiqBotsUI.tips.itemus.rare.R04)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cffa335ee"
	SynthiqBotsUI.itemus.rare = "R04"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("R05", 150, 0, "achievement_quests_completed_06", SynthiqBotsUI.tips.itemus.rare.R05)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cffff8000"
	SynthiqBotsUI.itemus.rare = "R05"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("R06", 180, 0, "achievement_quests_completed_07", SynthiqBotsUI.tips.itemus.rare.R06)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cffff0000"
	SynthiqBotsUI.itemus.rare = "R06"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("R07", 210, 0, "achievement_quests_completed_08", SynthiqBotsUI.tips.itemus.rare.R07)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Rare", pButton.texture)
	SynthiqBotsUI.itemus.color = "cffe6cc80"
	SynthiqBotsUI.itemus.rare = "R07"
	SynthiqBotsUI.itemus.addItems(1)
end

-- ITEMUS:SLOT --

SynthiqBotsUI.itemus.addButton("Slot", -94, 731, "inv_drink_18", SynthiqBotsUI.tips.itemus.slot.master)
.doLeft = function(pButton)
	SynthiqBotsUI.ShowHideSwitch(pButton.parent.frames["Slot"])
end

local tFrame = SynthiqBotsUI.itemus.addFrame("Slot", -61, 733)
tFrame:Hide()

tFrame.addButton("S00", 0, 0, "inv_drink_18", SynthiqBotsUI.tips.itemus.slot.S00)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S00"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S01", 30, 0, "inv_misc_desecrated_platehelm", SynthiqBotsUI.tips.itemus.slot.S01)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S01"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S02", 60, 0, "inv_jewelry_necklace_22", SynthiqBotsUI.tips.itemus.slot.S02)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S02"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S03", 90, 0, "inv_misc_desecrated_plateshoulder", SynthiqBotsUI.tips.itemus.slot.S03)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S03"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S04", 120, 0, "inv_shirt_grey_01", SynthiqBotsUI.tips.itemus.slot.S04)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S04"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S05", 150, 0, "inv_misc_desecrated_platechest", SynthiqBotsUI.tips.itemus.slot.S05)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S05"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S06", 180, 0, "inv_misc_desecrated_platebelt", SynthiqBotsUI.tips.itemus.slot.S06)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S06"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S07", 210, 0, "inv_misc_desecrated_platepants", SynthiqBotsUI.tips.itemus.slot.S07)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S07"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S08", 0, -30, "inv_misc_desecrated_plateboots", SynthiqBotsUI.tips.itemus.slot.S08)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S08"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S09", 30, -30, "inv_misc_desecrated_platebracer", SynthiqBotsUI.tips.itemus.slot.S09)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S09"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S10", 60, -30, "inv_misc_desecrated_plategloves", SynthiqBotsUI.tips.itemus.slot.S10)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S10"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S11", 90, -30, "inv_jewelry_ring_19", SynthiqBotsUI.tips.itemus.slot.S11)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S11"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S12", 120, -30, "inv_jewelry_ring_07", SynthiqBotsUI.tips.itemus.slot.S12)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S12"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S13", 150, -30, "inv_sword_23", SynthiqBotsUI.tips.itemus.slot.S13)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S13"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S14", 180, -30, "inv_shield_04", SynthiqBotsUI.tips.itemus.slot.S14)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S14"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S15", 210, -30, "inv_weapon_bow_05", SynthiqBotsUI.tips.itemus.slot.S15)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S15"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S16", 0, -60, "inv_misc_cape_20", SynthiqBotsUI.tips.itemus.slot.S16)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S16"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S17", 30, -60, "inv_axe_14", SynthiqBotsUI.tips.itemus.slot.S17)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S17"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S18", 60, -60, "inv_misc_bag_07_black", SynthiqBotsUI.tips.itemus.slot.S18)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S18"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S19", 90, -60, "inv_shirt_guildtabard_01", SynthiqBotsUI.tips.itemus.slot.S19)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S19"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S20", 120, -60, "inv_misc_desecrated_clothchest", SynthiqBotsUI.tips.itemus.slot.S20)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S20"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S21", 150, -60, "inv_hammer_07", SynthiqBotsUI.tips.itemus.slot.S21)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S21"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S22", 180, -60, "inv_sword_15", SynthiqBotsUI.tips.itemus.slot.S22)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S22"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S23", 210, -60, "inv_misc_book_09", SynthiqBotsUI.tips.itemus.slot.S23)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S23"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S24", 0, -90, "inv_misc_ammo_arrow_01", SynthiqBotsUI.tips.itemus.slot.S24)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S24"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S25", 30, -90, "inv_throwingknife_02", SynthiqBotsUI.tips.itemus.slot.S25)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S25"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S26", 60, -90, "inv_wand_07", SynthiqBotsUI.tips.itemus.slot.S26)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S26"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S27", 90, -90, "inv_misc_quiver_07", SynthiqBotsUI.tips.itemus.slot.S27)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S26"
	SynthiqBotsUI.itemus.addItems(1)
end

tFrame.addButton("S28", 120, -90, "inv_relics_idolofrejuvenation", SynthiqBotsUI.tips.itemus.slot.S28)
.doLeft = function(pButton)
	SynthiqBotsUI.Select(SynthiqBotsUI.itemus, "Slot", pButton.texture)
	SynthiqBotsUI.itemus.slot = "S28"
	SynthiqBotsUI.itemus.addItems(1)
end

-- ITEMUS:TYPE --

SynthiqBotsUI.itemus.addButton("Type", -94, 694, "inv_misc_head_clockworkgnome_01", SynthiqBotsUI.tips.itemus.type).setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.itemus.type = SynthiqBotsUI.IF(SynthiqBotsUI.OnOffSwitch(pButton), "NPC", "PC")
	SynthiqBotsUI.itemus.addItems(1)
end

-- ICONOS --

SynthiqBotsUI.iconos = SynthiqBotsUI.newFrame(SynthiqBotsUI, -860, -144, 32, 442, 884)
SynthiqBotsUI.iconos.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Iconos.blp")
SynthiqBotsUI.iconos.addText("Title", "Iconos", "CENTER", -57, 429, 13)
SynthiqBotsUI.iconos.addText("Pages", "0/0", "CENTER", -57, 409, 13)
SynthiqBotsUI.iconos.max = 1
SynthiqBotsUI.iconos.now = 1
SynthiqBotsUI.iconos:SetMovable(true)
SynthiqBotsUI.iconos:Hide()

SynthiqBotsUI.iconos.movButton("Move", -407, 850, 32, SynthiqBotsUI.tips.move.iconos)

SynthiqBotsUI.iconos.wowButton("<", -319, 841, 15, 18, 13).doHide()
.doLeft = function(pButton)
	SynthiqBotsUI.iconos.now = SynthiqBotsUI.iconos.now - 1
	SynthiqBotsUI.iconos.addIcons()
end

SynthiqBotsUI.iconos.wowButton(">", -225, 841, 15, 18, 13).doHide()
.doLeft = function(pButton)
	SynthiqBotsUI.iconos.now = SynthiqBotsUI.iconos.now + 1
	SynthiqBotsUI.iconos.addIcons()
end

SynthiqBotsUI.iconos.wowButton("X", -126, 862, 15, 18, 13)
.doLeft = function(pButton)
	SynthiqBotsUI.iconos:Hide()
end

local tFrame = SynthiqBotsUI.iconos.addFrame("Icons", -397, 807, 32)
tFrame:Show()

-- SPELLBOOK --

SynthiqBotsUI.spellbook = SynthiqBotsUI.newFrame(SynthiqBotsUI, -802, 302, 28, 336, 448)
SynthiqBotsUI.spellbook.spells = {}
SynthiqBotsUI.spellbook.icons = {}
SynthiqBotsUI.spellbook.max = 1
SynthiqBotsUI.spellbook.now = 1
SynthiqBotsUI.spellbook:SetMovable(true)
SynthiqBotsUI.spellbook:Hide()

for i = 1, GetNumMacroIcons() do SynthiqBotsUI.spellbook.icons[GetMacroIconInfo(i)] = i end

local tFrame = SynthiqBotsUI.spellbook.addFrame("Icon", -276, 392, 28, 50, 50)
tFrame.addTexture("Interface/Spellbook/Spellbook-Icon")
tFrame:SetFrameLevel(0)

local tFrame = SynthiqBotsUI.spellbook.addFrame("TopLeft", -112, 224, 28, 224, 224)
tFrame.addTexture("Interface/ItemTextFrame/UI-ItemText-TopLeft")
tFrame:SetFrameLevel(1)

local tFrame = SynthiqBotsUI.spellbook.addFrame("TopRight", -0, 224, 28, 112, 224)
tFrame.addTexture("Interface/Spellbook/UI-SpellbookPanel-TopRight")
tFrame:SetFrameLevel(2)

local tFrame = SynthiqBotsUI.spellbook.addFrame("BottomLeft", -112, 0, 28, 224, 224)
tFrame.addTexture("Interface/ItemTextFrame/UI-ItemText-BotLeft")
tFrame:SetFrameLevel(3)

local tFrame = SynthiqBotsUI.spellbook.addFrame("BottomRight", -0, 0, 28, 112, 224)
tFrame.addTexture("Interface/Spellbook/UI-SpellbookPanel-BotRight")
tFrame:SetFrameLevel(4)

local tOverlay = SynthiqBotsUI.spellbook.addFrame("Overlay", -47, 81, 28, 258, 292)
tOverlay.addText("Title", "Spellbook", "CENTER", 14, 200, 13)
tOverlay.addText("Pages", "0/0", "CENTER", 14, 173, 13)
tOverlay:SetFrameLevel(5)

tOverlay.movButton("Move", -226, 310, 50, SynthiqBotsUI.tips.move.spellbook, SynthiqBotsUI.spellbook)

tOverlay.wowButton("<", -159, 309, 15, 18, 13)
.doLeft = function(pButton)
	SynthiqBotsUI.spellbook.to = SynthiqBotsUI.spellbook.to - 16
	SynthiqBotsUI.spellbook.now = SynthiqBotsUI.spellbook.now - 1
	SynthiqBotsUI.spellbook.from = SynthiqBotsUI.spellbook.from - 16
	SynthiqBotsUI.spellbook.frames["Overlay"].setText("Pages", SynthiqBotsUI.spellbook.now .. "/" .. SynthiqBotsUI.spellbook.max)
	SynthiqBotsUI.spellbook.frames["Overlay"].buttons[">"].doShow()
	
	if(SynthiqBotsUI.spellbook.now == 1) then pButton.doHide() end
	local tIndex = 1
	
	for i = SynthiqBotsUI.spellbook.from, SynthiqBotsUI.spellbook.to do
		SynthiqBotsUI.setSpell(tIndex, SynthiqBotsUI.spellbook.spells[i], pButton.getName())
		tIndex = tIndex + 1
	end
end

tOverlay.wowButton(">", -59, 309, 15, 18, 11)
.doLeft = function(pButton)
	SynthiqBotsUI.spellbook.to = SynthiqBotsUI.spellbook.to + 16
	SynthiqBotsUI.spellbook.now = SynthiqBotsUI.spellbook.now + 1
	SynthiqBotsUI.spellbook.from = SynthiqBotsUI.spellbook.from + 16
	SynthiqBotsUI.spellbook.frames["Overlay"].setText("Pages", SynthiqBotsUI.spellbook.now .. "/" .. SynthiqBotsUI.spellbook.max)
	SynthiqBotsUI.spellbook.frames["Overlay"].buttons["<"].doShow()
	
	if(SynthiqBotsUI.spellbook.now == SynthiqBotsUI.spellbook.max) then pButton.doHide() end
	local tIndex = 1
	
	for i = SynthiqBotsUI.spellbook.from, SynthiqBotsUI.spellbook.to do
		SynthiqBotsUI.setSpell(tIndex, SynthiqBotsUI.spellbook.spells[i], pButton.getName())
		tIndex = tIndex + 1
	end
end

tOverlay.wowButton("X", 16, 336, 15, 18, 11)
.doLeft = function(pButton)
	local tUnits = SynthiqBotsUI.frames["MultiBar"].frames["Units"]
	local tButton = tUnits.frames[SynthiqBotsUI.spellbook.name].buttons["Spellbook"]
	tButton.doLeft(tButton)
end

tOverlay.addText("R01", "|cff402000Rank|r", "TOPLEFT", 44, -16, 11)
tOverlay.addText("T01", "|cffffcc00Title|r", "TOPLEFT", 30, -2, 12)
local tButton = tOverlay.addButton("S01", -230, 264, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R02", "|cff402000Rank|r", "TOPLEFT", 172, -16, 11)
tOverlay.addText("T02", "|cffffcc00Title|r", "TOPLEFT", 159, -2, 12)
local tButton = tOverlay.addButton("S02", -101, 264, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R03", "|cff402000Rank|r", "TOPLEFT", 44, -52, 11)
tOverlay.addText("T03", "|cffffcc00Title|r", "TOPLEFT", 30, -38, 12)
local tButton = tOverlay.addButton("S03", -230, 228, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R04", "|cff402000Rank|r", "TOPLEFT", 172, -52, 11)
tOverlay.addText("T04", "|cffffcc00Title|r", "TOPLEFT", 159, -38, 12)
local tButton = tOverlay.addButton("S04", -101, 228, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R05", "|cff402000Rank|r", "TOPLEFT", 44, -88, 11)
tOverlay.addText("T05", "|cffffcc00Title|r", "TOPLEFT", 30, -74, 12)
local tButton = tOverlay.addButton("S05", -230, 192, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R06", "|cff402000Rank|r", "TOPLEFT", 172, -88, 11)
tOverlay.addText("T06", "|cffffcc00Title|r", "TOPLEFT", 159, -74, 12)
local tButton = tOverlay.addButton("S06", -101, 192, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R07", "|cff402000Rank|r", "TOPLEFT", 44, -124, 11)
tOverlay.addText("T07", "|cffffcc00Title|r", "TOPLEFT", 30, -110, 12)
local tButton = tOverlay.addButton("S07", -230, 156, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R08", "|cff402000Rank|r", "TOPLEFT", 172, -124, 11)
tOverlay.addText("T08", "|cffffcc00Title|r", "TOPLEFT", 159, -110, 12)
local tButton = tOverlay.addButton("S08", -101, 156, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R09", "|cff402000Rank|r", "TOPLEFT", 44, -160, 11)
tOverlay.addText("T09", "|cffffcc00Title|r", "TOPLEFT", 30, -146, 12)
local tButton = tOverlay.addButton("S09", -230, 120, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R10", "|cff402000Rank|r", "TOPLEFT", 172, -160, 11)
tOverlay.addText("T10", "|cffffcc00Title|r", "TOPLEFT", 159, -146, 12)
local tButton = tOverlay.addButton("S10", -101, 120, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R11", "|cff402000Rank|r", "TOPLEFT", 44, -196, 11)
tOverlay.addText("T11", "|cffffcc00Title|r", "TOPLEFT", 30, -182, 12)
local tButton = tOverlay.addButton("S11", -230, 84, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R12", "|cff402000Rank|r", "TOPLEFT", 172, -196, 11)
tOverlay.addText("T12", "|cffffcc00Title|r", "TOPLEFT", 159, -182, 12)
local tButton = tOverlay.addButton("S12", -101, 84, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R13", "|cff402000Rank|r", "TOPLEFT", 44, -232, 11)
tOverlay.addText("T13", "|cffffcc00Title|r", "TOPLEFT", 30, -218, 12)
local tButton = tOverlay.addButton("S13", -230, 48, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R14", "|cff402000Rank|r", "TOPLEFT", 172, -232, 11)
tOverlay.addText("T14", "|cffffcc00Title|r", "TOPLEFT", 159, -218, 12)
local tButton = tOverlay.addButton("S14", -101, 48, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R15", "|cff402000Rank|r", "TOPLEFT", 44, -268, 11)
tOverlay.addText("T15", "|cffffcc00Title|r", "TOPLEFT", 30, -254, 12)
local tButton = tOverlay.addButton("S15", -230, 12, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.addText("R16", "|cff402000Rank|r", "TOPLEFT", 172, -268, 11)
tOverlay.addText("T16", "|cffffcc00Title|r", "TOPLEFT", 159, -254, 12)
local tButton = tOverlay.addButton("S16", -101, 12, "inv_misc_questionmark", "Text")
tButton.doRight = function(pButton)
	SynthiqBotsUI.SpellToMacro(SynthiqBotsUI.spellbook.name, pButton.spell, pButton.texture)
end
tButton.doLeft = function(pButton)
	SendChatMessage("cast " .. pButton.spell, "WHISPER", nil, SynthiqBotsUI.spellbook.name)
end

tOverlay.boxButton("C01", -214, 262, 16, true)
tOverlay.boxButton("C02",  -85, 262, 16, true)
tOverlay.boxButton("C03", -214, 226, 16, true)
tOverlay.boxButton("C04",  -85, 226, 16, true)
tOverlay.boxButton("C05", -214, 190, 16, true)
tOverlay.boxButton("C06",  -85, 190, 16, true)
tOverlay.boxButton("C07", -214, 154, 16, true)
tOverlay.boxButton("C08",  -85, 154, 16, true)
tOverlay.boxButton("C09", -214, 118, 16, true)
tOverlay.boxButton("C10",  -85, 118, 16, true)
tOverlay.boxButton("C11", -214,  82, 16, true)
tOverlay.boxButton("C12",  -85,  82, 16, true)
tOverlay.boxButton("C13", -214,  46, 16, true)
tOverlay.boxButton("C14",  -85,  46, 16, true)
tOverlay.boxButton("C15", -214,  10, 16, true)
tOverlay.boxButton("C16",  -85,  10, 16, true)

-- REWARD --

SynthiqBotsUI.reward = SynthiqBotsUI.newFrame(SynthiqBotsUI, -754, 238, 28, 384, 512)
SynthiqBotsUI.reward.rewards = {}
SynthiqBotsUI.reward.units = {}
SynthiqBotsUI.reward.from = 1
SynthiqBotsUI.reward.max = 1
SynthiqBotsUI.reward.now = 1
SynthiqBotsUI.reward.to = 12
SynthiqBotsUI.reward:SetMovable(true)
SynthiqBotsUI.reward:Hide()

SynthiqBotsUI.reward.doClose = function()
	local tOverlay = SynthiqBotsUI.reward.frames["Overlay"]
	for key, value in pairs(SynthiqBotsUI.reward.units) do if(value.rewarded == false) then return end end
	SynthiqBotsUI.reward:Hide()
end

local tFrame = SynthiqBotsUI.reward.addFrame("Icon", -313, 443, 28, 64, 64)
tFrame.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Reward.blp")
tFrame:SetFrameLevel(0)

local tFrame = SynthiqBotsUI.reward.addFrame("TopLeft", -128, 256, 28, 256, 256)
tFrame.addTexture("Interface/ItemTextFrame/UI-ItemText-TopLeft")
tFrame:SetFrameLevel(1)

local tFrame = SynthiqBotsUI.reward.addFrame("TopRight", -0, 256, 28, 128, 256)
tFrame.addTexture("Interface/Spellbook/UI-SpellbookPanel-TopRight")
tFrame:SetFrameLevel(2)

local tFrame = SynthiqBotsUI.reward.addFrame("BottomLeft", -128, 0, 28, 256, 256)
tFrame.addTexture("Interface/ItemTextFrame/UI-ItemText-BotLeft")
tFrame:SetFrameLevel(3)

local tFrame = SynthiqBotsUI.reward.addFrame("BottomRight", -0, 0, 28, 128, 256)
tFrame.addTexture("Interface/Spellbook/UI-SpellbookPanel-BotRight")
tFrame:SetFrameLevel(4)

local tOverlay = SynthiqBotsUI.reward.addFrame("Overlay", -48, 97, 28, 310, 330)
tOverlay.addText("Title", SynthiqBotsUI.info.reward, "CENTER", 16, 226, 13)
tOverlay.addText("Pages", "0/0", "CENTER", 16, 196, 13)
tOverlay:SetFrameLevel(5)

tOverlay.movButton("Move", -270, 354, 50, SynthiqBotsUI.tips.move.reward, SynthiqBotsUI.reward)

tOverlay.wowButton("<", -182, 351, 15, 18, 13)
.doLeft = function(pButton)
	local tOverlay = SynthiqBotsUI.reward.frames["Overlay"]
	local tReward = SynthiqBotsUI.reward
	
	tReward.to = tReward.to - 12
	tReward.now = tReward.now - 1
	tReward.from = tReward.from - 12
	tOverlay.setText("Pages", tReward.now .. "/" .. tReward.max)
	tOverlay.buttons[">"].doShow()
	
	if(tReward.now == 1) then pButton.doHide() end
	local tIndex = 1
	
	for i = tReward.from, tReward.to do
		SynthiqBotsUI.setReward(tIndex, SynthiqBotsUI.reward.units[i])
		tIndex = tIndex + 1
	end
end

tOverlay.wowButton(">", -82, 351, 15, 18, 11)
.doLeft = function(pButton)
	local tOverlay = SynthiqBotsUI.reward.frames["Overlay"]
	local tReward = SynthiqBotsUI.reward
	
	tReward.to = tReward.to + 12
	tReward.now = tReward.now + 1
	tReward.from = tReward.from + 12
	tOverlay.setText("Pages", tReward.now .. "/" .. tReward.max)
	tOverlay.buttons["<"].doShow()
	
	if(tReward.now == tReward.max) then pButton.doHide() end
	local tIndex = 1
	
	for i = tReward.from, tReward.to do
		SynthiqBotsUI.setReward(tIndex, SynthiqBotsUI.reward.units[i])
		tIndex = tIndex + 1
	end
end

tOverlay.wowButton("X", 13, 381, 17, 20, 11)
.doLeft = function(pButton)
	SynthiqBotsUI.reward:Hide()
end

-- GROUP:U01 --

local tFrame = tOverlay.addFrame("U01", -156, 282, 23, 154, 48)
tFrame.addText("U01", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U02 --

local tFrame = tOverlay.addFrame("U02", 0, 282, 23, 154, 48)
tFrame.addText("U02", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U03 --

local tFrame = tOverlay.addFrame("U03", -156, 228, 23, 154, 48)
tFrame.addText("U03", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U04 --

local tFrame = tOverlay.addFrame("U04", 0, 228, 23, 154, 48)
tFrame.addText("U04", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U05 --

local tFrame = tOverlay.addFrame("U05", -156, 174, 23, 154, 48)
tFrame.addText("U05", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U06 --

local tFrame = tOverlay.addFrame("U06", 0, 174, 23, 154, 48)
tFrame.addText("U06", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U07 --

local tFrame = tOverlay.addFrame("U07", -156, 120, 23, 154, 48)
tFrame.addText("U07", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U08 --

local tFrame = tOverlay.addFrame("U08", 0, 120, 23, 154, 48)
tFrame.addText("U08", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U09 --

local tFrame = tOverlay.addFrame("U09", -156, 66, 23, 154, 48)
tFrame.addText("U09", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U10 --

local tFrame = tOverlay.addFrame("U10", 0, 66, 23, 154, 48)
tFrame.addText("U10", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U11 --

local tFrame = tOverlay.addFrame("U11", -156, 12, 23, 154, 48)
tFrame.addText("U11", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- GROUP:U12 --

local tFrame = tOverlay.addFrame("U12", 0, 12, 23, 154, 48)
tFrame.addText("U12", "|cffffcc00NAME - CLASS|r", "BOTTOMLEFT", 20, 28, 13)
tFrame.addButton("R1", -130, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R2", -104, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R3", -78, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R4", -52, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R5", -26, 0, "inv_misc_questionmark", "Text")
tFrame.addButton("R6", -0, 0, "inv_misc_questionmark", "Text")
tFrame.addFrame("Inspector", -137, 26, 16)
.addButton("Inspect", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\filter_none.blp", "Inspect")
.doLeft = function(pButton)
	InspectUnit(pButton.getName())
end

-- TALENT --

SynthiqBotsUI.talent = SynthiqBotsUI.newFrame(SynthiqBotsUI, -104, -276, 28, 1024, 1024)
SynthiqBotsUI.talent.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent.blp")
SynthiqBotsUI.talent.addText("Points", SynthiqBotsUI.info.talent["Points"], "CENTER", -228, -8, 13)
SynthiqBotsUI.talent.addText("Title", SynthiqBotsUI.info.talent["Title"], "CENTER", -228, 491, 13)
SynthiqBotsUI.talent:SetMovable(true)
SynthiqBotsUI.talent:Hide()

SynthiqBotsUI.talent.movButton("Move", -960, 960, 64, SynthiqBotsUI.tips.move.talent)

SynthiqBotsUI.talent.wowButton(SynthiqBotsUI.info.talent.Apply, -474, 966, 100, 20, 12).doHide()
.doLeft = function(pButton)
	local tValues = ""
	
	for i = 1, 3 do
		local tTab = SynthiqBotsUI.talent.frames["Tab" .. i]
		
		for j = 1, table.getn(tTab.buttons) do
			tValues = tValues .. tTab.buttons[j].value
		end
		
		if(i < 3) then tValues = tValues .. "-" end
	end
	
	SendChatMessage("talents apply " ..tValues, "WHISPER", nil, SynthiqBotsUI.talent.name)
	pButton.doHide()
end

SynthiqBotsUI.talent.wowButton(SynthiqBotsUI.info.talent.Copy, -854, 966, 100, 20, 12)
.doLeft = function(pButton)
	local tName = UnitName("target")
	if(tName == nil or tName == "Unknown Entity") then return SendChatMessage(SynthiqBotsUI.info.target, "SAY") end
	
	local tLocClass, tClass = UnitClass("target")
	if(SynthiqBotsUI.talent.class ~= SynthiqBotsUI.toClass(tClass)) then return SendChatMessage("The Classes do not match.", "SAY") end
	
	local tUnit = SynthiqBotsUI.toUnit(SynthiqBotsUI.talent.name)
	if(UnitLevel(tUnit) ~= UnitLevel("target")) then return SendChatMessage("The Levels do not match.", "SAY") end
	
	local tValues = ""
	
	for i = 1, 3 do
		local tTab = SynthiqBotsUI.talent.frames["Tab" .. i]
		
		for j = 1, table.getn(tTab.buttons) do
			tValues = tValues .. tTab.buttons[j].value
		end
		
		if(i < 3) then tValues = tValues .. "-" end
	end

	SendChatMessage("talents apply " ..tValues, "WHISPER", nil, tName)
end

SynthiqBotsUI.talent.wowButton("X", -470, 992, 17, 20, 13)
.doLeft = function(pButton)
	local tUnits = SynthiqBotsUI.frames["MultiBar"].frames["Units"]
	local tButton = tUnits.frames[SynthiqBotsUI.talent.name].buttons["Talent"]
	tButton.doLeft(tButton)
end

local tTab = SynthiqBotsUI.talent.addFrame("Tab1", -830, 518, 28, 170, 408)
tTab.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\White.blp")
tTab.addText("Title", "Title", "CENTER", 0, 214, 13)
tTab.arrows = {}
tTab.value = 0
tTab.id = 1

local tTab = SynthiqBotsUI.talent.addFrame("Tab2", -656, 518, 28, 170, 408)
tTab.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\White.blp")
tTab.addText("Title", "Title", "CENTER", 0, 214, 13)
tTab.arrows = {}
tTab.value = 0
tTab.id = 2

local tTab = SynthiqBotsUI.talent.addFrame("Tab3", -482, 518, 28, 170, 408)
tTab.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\White.blp")
tTab.addText("Title", "Title", "CENTER", 0, 214, 13)
tTab.arrows = {}
tTab.value = 0
tTab.id = 3

--[[
local tTab = SynthiqBotsUI.talent.addFrame("Tab4", -513, 518, 28, 456, 430)
tTab.addFrame("Glow", 0, 0, 28, 456, 430).setAlpha(0.5).doHide().addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Glow.blp")
tTab.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs.blp")
tTab:Hide()

-- GLYPH:SOCKET1 --

-- Level 15
local tGlyph = tTab.addFrame("Socket1", -176.5, 324, 102)
tGlyph.addFrame("Glow",   0,  0, 102).setLevel(7).doHide().addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Major.blp")
tGlyph.addFrame("Rune", -29, 29,  44).setLevel(8).setAlpha(0.7).doHide().addTexture("Interface/Spellbook/UI-Glyph-Rune-1")
tGlyph.type = "Major"
tGlyph.item = 0

tGlyph.addFrame("Overlay", -10, 10, 82).setLevel(9).doHide()
.addButton("Rune", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Major_Socket.blp", "")
.setHighlight("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Highlight.blp")
.doLeft = function(pButton)
	local tType, tItem, tLink = GetCursorInfo()
	if(tType ~= "item") then return end
	
	local tGlyph = SynthiqBotsUI.doSplit(SynthiqBotsUI.data.talent.glylphs[pButton.getClass()][pButton.parent.parent.type][cItem], ", ")
	if(tGlyph == nil) then return SendChatMessage("This Glyph is not allowed in this Socket.", "SAY") end
	
	local tLevel = UnitLevel(SynthiqBotsUI.toUnit(pButton.getName()))
	if(tGlyph[1] > tLevel) then return SendChatMessage("The Level of the Bot is to low.", "SAY") end
	
	local tSocket = pButton.parent.parent
	tSocket.frames["Glow"].doShow()
	tSocket.frames["Rune"].doShow().setTexture("Interface/Spellbook/UI-Glyph-Rune" .. tGlyph[2])
	tSocket.item = tItem
	DeleteCursorItem()
end

-- GLYPH:SOCKET2 --

-- Level 15
local tGlyph = tTab.addFrame("Socket2", -187, 18.5, 82)
tGlyph.addFrame("Glow",   0,  0, 82).setLevel(7).doHide().addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Minor.blp")
tGlyph.addFrame("Rune", -25, 25, 32).setLevel(8).setAlpha(0.7).doHide().addTexture("Interface/Spellbook/UI-Glyph-Rune-1")
tGlyph.type = "Minor"

tGlyph.addFrame("Overlay", -7, 7, 68).setLevel(9).doHide()
.addButton("Rune", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Minor_Socket.blp", "")
.setHighlight("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Highlight.blp")
.doLeft = function(pButton)
end

-- GLYPH:SOCKET3 --

-- Level 30
local tGlyph = tTab.addFrame("Socket3", -18.5, 50.5, 102)
tGlyph.addFrame("Glow",   0,  0, 102).setLevel(7).doHide().addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Major.blp")
tGlyph.addFrame("Rune", -29, 29,  44).setLevel(8).setAlpha(0.7).doHide().addTexture("Interface/Spellbook/UI-Glyph-Rune-1")
tGlyph.type = "Major"

tGlyph.addFrame("Overlay", -10, 10, 82).setLevel(9).doHide()
.addButton("Rune", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Major_Socket.blp", "")
.setHighlight("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Highlight.blp")
.doLeft = function(pButton)
end

-- GLYPH:SOCKET4 --

-- Level 50
local tGlyph = tTab.addFrame("Socket4", -302.5, 218, 82)
tGlyph.addFrame("Glow",   0,  0, 82).setLevel(7).doHide().addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Minor.blp")
tGlyph.addFrame("Rune", -25, 25, 32).setLevel(8).setAlpha(0.7).doHide().addTexture("Interface/Spellbook/UI-Glyph-Rune-1")
tGlyph.type = "Minor"

tGlyph.addFrame("Overlay", -7, 7, 68).setLevel(9).doHide()
.addButton("Rune", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Minor_Socket.blp", "")
.setHighlight("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Highlight.blp")
.doLeft = function(pButton)
end

-- GLYPH:SOCKET5 --

-- Level 70
local tGlyph = tTab.addFrame("Socket5", -72.5, 218, 82)
tGlyph.addFrame("Glow",   0,  0, 82).setLevel(7).doHide().addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Minor.blp")
tGlyph.addFrame("Rune", -25, 25, 32).setLevel(8).setAlpha(0.7).doHide().addTexture("Interface/Spellbook/UI-Glyph-Rune-1")
tGlyph.type = "Minor"

tGlyph.addFrame("Overlay", -7, 7, 68).setLevel(9).doHide()
.addButton("Rune", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Minor_Socket.blp", "")
.setHighlight("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Highlight.blp")
.doLeft = function(pButton)
end

-- GLYPH:SOCKET6 --

-- Level 80
local tGlyph = tTab.addFrame("Socket6", -336, 50.5, 102)
tGlyph.addFrame("Glow",   0,  0, 102).setLevel(7).doHide().addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Major.blp")
tGlyph.addFrame("Rune", -29, 29,  44).setLevel(8).setAlpha(0.7).doHide().addTexture("Interface/Spellbook/UI-Glyph-Rune-1")
tGlyph.type = "Major"

tGlyph.addFrame("Overlay", -10, 10, 82).setLevel(9).doHide()
.addButton("Rune", 0, 0, "Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Major_Socket.blp", "")
.setHighlight("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Glyphs_Highlight.blp")
.doLeft = function(pButton)
end

local tTab = SynthiqBotsUI.talent.addFrame("Tab5", -900, 461, 28, 96, 24)
tTab.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Tab.blp")
tTab.wowButton("Talents", -2, 6, 92, 17, 11)
.doLeft = function(pButton)
	SynthiqBotsUI.talent.setText("Title", SynthiqBotsUI.doReplace(SynthiqBotsUI.info.talent.Title, "NAME", SynthiqBotsUI.talent.name))
	SynthiqBotsUI.talent.texts["Points"]:Show()
	SynthiqBotsUI.talent.frames["Tab1"]:Show()
	SynthiqBotsUI.talent.frames["Tab2"]:Show()
	SynthiqBotsUI.talent.frames["Tab3"]:Show()
	SynthiqBotsUI.talent.frames["Tab4"]:Hide()
end

local tTab = SynthiqBotsUI.talent.addFrame("Tab6", -800, 461, 28, 96, 24)
tTab.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Tab.blp")
tTab.wowButton("Glyphs", -2, 6, 92, 17, 11)
.doLeft = function(pButton)
	SynthiqBotsUI.talent.setText("Title", "Glyphs of " .. SynthiqBotsUI.talent.name)
	SynthiqBotsUI.talent.texts["Points"]:Hide()
	SynthiqBotsUI.talent.frames["Tab1"]:Hide()
	SynthiqBotsUI.talent.frames["Tab2"]:Hide()
	SynthiqBotsUI.talent.frames["Tab3"]:Hide()
	SynthiqBotsUI.talent.frames["Tab4"]:Show()
end
]]--

SynthiqBotsUI.talent.setGrid = function(pTab)
	pTab.grid = {}
	pTab.grid.icons = {}
	pTab.grid.icons.size = pTab.size + 8
	pTab.grid.icons.x = pTab.width / 2 + pTab.grid.icons.size * 2 + 4
	pTab.grid.icons.y = pTab.height / 2 + pTab.grid.icons.size * 5.5 + 4
	pTab.grid.arrows = {}
	pTab.grid.arrows.size = pTab.grid.icons.size + 8
	pTab.grid.arrows.x = pTab.width / 2 + pTab.grid.icons.size * 2 - 4
	pTab.grid.arrows.y = pTab.height / 2 + pTab.grid.icons.size * 5.5 - 4
	pTab.grid.values = {}
	pTab.grid.values.x = pTab.width / 2 + pTab.grid.icons.size * 2
	pTab.grid.values.y = pTab.height / 2 + pTab.grid.icons.size * 5.5
	return pTab
end

SynthiqBotsUI.talent.addArrow = function(pTab, pID, pNeeds, piX, piY, pTexture)
	local tArrow = pTab.addFrame("Arrow" .. pID, piX * pTab.grid.icons.size - pTab.grid.arrows.x, pTab.grid.arrows.y - piY * pTab.grid.icons.size, pTab.grid.arrows.size)
	tArrow.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Silver_" .. pTexture .. ".blp")
	tArrow.active = "Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Gold_" .. pTexture .. ".blp"
	tArrow.needs = pNeeds
	tArrow:SetFrameLevel(7)
	return tArrow
end

SynthiqBotsUI.talent.addTalent = function(pTab, pID, pNeeds, pValue, pMax, piX, piY, pTexture, pTips)
	local tTalent = pTab.addButton(pID, piX * pTab.grid.icons.size - pTab.grid.icons.x, pTab.grid.icons.y - piY * pTab.grid.icons.size, pTexture, pTips[pValue + 1])
	tTalent.points = piY * 5 - 5
	tTalent.needs = pNeeds
	tTalent.value = pValue
	tTalent.tips = pTips
	tTalent.max = pMax
	tTalent.id = pID
	
	tTalent.doLeft = function(pButton)
		if(SynthiqBotsUI.talent.points == 0) then return end
		
		local tButtons = pButton.parent.buttons
		local tValue = pButton.parent.frames[pButton.id]
		local tTab = pButton.parent
		
		if(pButton.state == false) then return end
		if(pButton.value == pButton.max) then return end
		if(pButton.needs > 0 and tButtons[pButton.needs].value == 0) then return end
		
		SynthiqBotsUI.talent.points = SynthiqBotsUI.talent.points - 1
		SynthiqBotsUI.talent.setText("Points", SynthiqBotsUI.info.talent["Points"] .. SynthiqBotsUI.talent.points)
		
		tTab.value = tTab.value + 1
		tTab.setText("Title", SynthiqBotsUI.info.talent[pButton.getClass() .. tTab.id] .. " ("  .. tTab.value .. ")")
		
		pButton.value = pButton.value + 1
		pButton.tip = pButton.tips[pButton.value + 1]
		
		local tColor = SynthiqBotsUI.IF(pButton.value < pButton.max, "|cff4db24d", "|cffffcc00")
		tValue.setText("Value", tColor .. pButton.value .. "/" .. pButton.max .. "|r")
		tValue:Show()
		
		for i = 1, table.getn(tButtons) do
			if(tButtons[i].points > tTab.value)
			then tButtons[i].setDisable()
			else
				if(tButtons[i].needs > 0)
				then if(tButtons[tButtons[i].needs].value > 0) then tButtons[i].setEnable() end
				else tButtons[i].setEnable()
				end
			end
		end
		
		SynthiqBotsUI.talent.buttons[SynthiqBotsUI.info.talent.Apply].doShow()
		SynthiqBotsUI.talent.doState()
	end
	
	tTalent:SetFrameLevel(8)
	return tTalent
end

SynthiqBotsUI.talent.addValue = function(pTab, pID, piX, piY, pRank, pMax)
	local tColor = SynthiqBotsUI.IF(pRank > 0, SynthiqBotsUI.IF(pRank < pMax, "|cff4db24d", "|cffffcc00"), "|cffffffff")
	local tValue = pTab.addFrame(pID, piX * pTab.grid.icons.size - pTab.grid.values.x, pTab.grid.values.y - piY * pTab.grid.icons.size, 24, 18, 12)
	tValue.addTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_Black.blp")
	tValue.addText("Value", tColor .. pRank .. "/" .. pMax .. "|r", "CENTER", -0.5, 1, 10)
	if(SynthiqBotsUI.talent.points == 0 and pRank == 0) then tValue:Hide() end
	tValue:SetFrameLevel(9)
	return tValue
end

SynthiqBotsUI.talent.setTalents = function()
	local tClass = SynthiqBotsUI.data.talent.talents[SynthiqBotsUI.talent.class]
	local tArrow = SynthiqBotsUI.data.talent.arrows[SynthiqBotsUI.talent.class]
	
	SynthiqBotsUI.talent.points = tonumber(GetUnspentTalentPoints(true))
	SynthiqBotsUI.talent.setText("Points", SynthiqBotsUI.info.talent["Points"] .. SynthiqBotsUI.talent.points)
	SynthiqBotsUI.talent.setText("Title", SynthiqBotsUI.doReplace(SynthiqBotsUI.info.talent["Title"], "NAME", SynthiqBotsUI.talent.name))
	
	for i = 1, 3 do
		local tMarker = SynthiqBotsUI.talent.class .. i
		local tTab = SynthiqBotsUI.talent.setGrid(SynthiqBotsUI.talent.frames["Tab" .. i])
		tTab.setTexture("Interface\\AddOns\\synthiqbots-ui\\Textures\\Talent_" .. tMarker .. ".blp")
		tTab.value = 0
		tTab.id = i
		
		for j = 1, table.getn(tArrow[i]) do
			local tData = SynthiqBotsUI.doSplit(tArrow[i][j], ", ")
			local tNeed = tonumber(tData[1])
			tTab.arrows[j] = SynthiqBotsUI.talent.addArrow(tTab, j, tNeed, tData[2], tData[3], tData[4])
		end
		
		for j = 1, table.getn(tClass[i]) do
			local tTale = SynthiqBotsUI.doSplit(SynthiqBotsUI.doSplit(GetTalentLink(i, j, true), "|")[3], ":")[2]
			local iName, iIcon, iTier, iColumn, iRank = GetTalentInfo(i, j, true)
			local tData = SynthiqBotsUI.doSplit(tClass[i][j], ", ")
			local tMaxi = table.getn(tData) - 4
			local tNeed = tonumber(tData[1])
			local tRank = tonumber(iRank)
			local tTips = {}
			
			tTab.value = tTab.value + tRank
			table.insert(tTips, "|cff4e96f7|Htalent:" .. tTale ..":-1|h[" .. iName .. "]|h|r")
			for k = 5, table.getn(tData) do	table.insert(tTips, "|cff4e96f7|Htalent:" .. tTale ..":" .. (k - 5) .. "|h[" .. iName .. "]|h|r") end
			
			SynthiqBotsUI.talent.addTalent(tTab, j, tNeed, tRank, tMaxi, tData[2], tData[3], tData[4], tTips)
			SynthiqBotsUI.talent.addValue(tTab, j, tData[2], tData[3], tRank, tMaxi)
		end
		
		tTab.setText("Title", SynthiqBotsUI.info.talent[tMarker] .. " (" .. tTab.value .. ")")
	end
	
	SynthiqBotsUI.talent.doState()
	SynthiqBotsUI.talent:Show()
end

SynthiqBotsUI.talent.doState = function()
	for i = 1, 3 do
		local tTab = SynthiqBotsUI.talent.frames["Tab" .. i]
		
		for j = 1, table.getn(tTab.buttons) do
			local tTalent = tTab.buttons[j]
			local tValue = tTab.frames[j]
			
			if(SynthiqBotsUI.talent.points == 0) then
				if(tTalent.value == 0) then
					tTalent.setDisable(false)
					tValue:Hide()
				else
					tTalent.setEnable(false)
					tValue:Show()
				end
			else
				if(tTab.value < tTalent.points) then
					tTalent.setDisable(false)
					tValue:Hide()
				else
					tTalent.setEnable(false)
					tValue:Show()
				end
			end
		end
		
		for j = 1, table.getn(tTab.arrows) do
			if(tTab.buttons[tTab.arrows[j].needs].value > 0) then
				tTab.arrows[j].setTexture(tTab.arrows[j].active)
			end
		end
	end
end

SynthiqBotsUI.talent.doClear = function()
	for i = 1, 3 do
		local tTab = SynthiqBotsUI.talent.frames["Tab" .. i]
		for j = 1, table.getn(tTab.buttons) do tTab.buttons[j]:Hide() end
		for j = 1, table.getn(tTab.frames) do tTab.frames[j]:Hide() end
		for j = 1, table.getn(tTab.arrows) do tTab.arrows[j]:Hide() end
		table.wipe(tTab.buttons)
		table.wipe(tTab.frames)
		table.wipe(tTab.arrows)
		tTab.buttons = {}
		tTab.frames = {}
		tTab.arrows = {}
	end
end

-- RTSC --

local tRTSC = tMultiBar.addFrame("RTSC", -2, -34, 32).doHide()

local tButton = tRTSC.addButton("RTSC", 0, 0, "ability_hunter_markedfordeath", SynthiqBotsUI.tips.rtsc.master, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm")
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("co +rtsc,+guard,?")
	SynthiqBotsUI.ActionToGroup("nc +rtsc,+guard,?")
end
tButton.doLeft = function(pButton)
	local tFrame = pButton.parent.frames["Selector"]
	tFrame.doReset(tFrame)
end

-- RTSC:STORAGE --

local tSelector = tRTSC.addFrame("Selector", 0, 2, 28)
tSelector.selector = ""

tSelector.doExecute = function(pButton, pAction)
	if(pButton.parent.selector == "") then return SynthiqBotsUI.ActionToGroup(pAction) end
	local tGroups = SynthiqBotsUI.doSplit(pButton.parent.selector, " ")
	
	for i = 1, table.getn(tGroups) do
		SynthiqBotsUI.ActionToGroup(tGroups[i] .. " " .. pAction)
		pButton.parent.buttons[tGroups[i]].setDisable()
	end
	
	pButton.parent.selector = ""
end

tSelector.doSelect = function(pButton, pSelector)
	if(pButton.parent.selector == "")
	then pButton.parent.selector = pSelector
	else pButton.parent.selector = pButton.parent.selector .. " " .. pSelector
	end
end

tSelector.doReset = function(pFrame)
	if(pFrame.selector == "") then return end
	local tGroups = SynthiqBotsUI.doSplit(pFrame.selector, " ")
	for i = 1, table.getn(tGroups) do pFrame.buttons[tGroups[i]].setDisable() end
	pFrame.selector = ""
end

tSelector.addButton("MACRO9", -34, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 9")
	pButton.parent.buttons["RTSC9"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC9", -34, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 9")
	pButton.parent.buttons["MACRO9"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 9")
end

tSelector.addButton("MACRO8", -64, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 8")
	pButton.parent.buttons["RTSC8"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC8", -64, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 8")
	pButton.parent.buttons["MACRO8"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 8")
end

tSelector.addButton("MACRO7", -94, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 7")
	pButton.parent.buttons["RTSC7"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC7", -94, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 7")
	pButton.parent.buttons["MACRO7"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 7")
end

tSelector.addButton("MACRO6", -124, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 6")
	pButton.parent.buttons["RTSC6"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC6", -124, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 6")
	pButton.parent.buttons["MACRO6"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 6")
end

tSelector.addButton("MACRO5", -154, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 5")
	pButton.parent.buttons["RTSC5"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC5", -154, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 5")
	pButton.parent.buttons["MACRO5"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 5")
end

tSelector.addButton("MACRO4", -184, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 4")
	pButton.parent.buttons["RTSC4"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC4", -184, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 4")
	pButton.parent.buttons["MACRO4"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 4")
end

tSelector.addButton("MACRO3", -214, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 3")
	pButton.parent.buttons["RTSC3"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC3", -214, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 3")
	pButton.parent.buttons["MACRO3"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 3")
end

tSelector.addButton("MACRO2", -244, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 2")
	pButton.parent.buttons["RTSC2"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC2", -244, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 2")
	pButton.parent.buttons["MACRO2"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 2")
end

tSelector.addButton("MACRO1", -274, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.macro, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc save 1")
	pButton.parent.buttons["RTSC1"].doShow()
	pButton.doHide()
end

local tButton = tSelector.addButton("RTSC1", -274, 0, "achievement_bg_winwsg_3-0", SynthiqBotsUI.tips.rtsc.spot, "SecureActionButtonTemplate").doHide()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc unsave 1")
	pButton.parent.buttons["MACRO1"].doShow()
	pButton.doHide()
end
tButton.doLeft = function(pButton)
	pButton.parent.doExecute(pButton, "rtsc go 1")
end

-- RTSC:SELECTOR --

local tButton = tSelector.addButton("@group1", 30, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_group1.blp", SynthiqBotsUI.tips.rtsc.group1, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").doHide().setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group1 rtsc select")
	pButton.parent.doSelect(pButton, "@group1")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group1 rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@group2", 60, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_group2.blp", SynthiqBotsUI.tips.rtsc.group2, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").doHide().setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group2 rtsc select")
	pButton.parent.doSelect(pButton, "@group2")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group2 rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@group3", 90, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_group3.blp", SynthiqBotsUI.tips.rtsc.group3, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").doHide().setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group3 rtsc select")
	pButton.parent.doSelect(pButton, "@group3")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group3 rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@group4", 120, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_group4.blp", SynthiqBotsUI.tips.rtsc.group4, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").doHide().setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group4 rtsc select")
	pButton.parent.doSelect(pButton, "@group4")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group4 rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@group5", 150, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_group5.blp", SynthiqBotsUI.tips.rtsc.group5, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").doHide().setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group5 rtsc select")
	pButton.parent.doSelect(pButton, "@group5")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@group5 rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@tank", 30, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_tank.blp", SynthiqBotsUI.tips.rtsc.tank, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@tank rtsc select")
	pButton.parent.doSelect(pButton, "@tank")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@tank rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@dps", 60, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_dps.blp", SynthiqBotsUI.tips.rtsc.dps, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@dps rtsc select")
	pButton.parent.doSelect(pButton, "@dps")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@dps rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@healer", 90, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_healer.blp", SynthiqBotsUI.tips.rtsc.healer, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@healer rtsc select")
	pButton.parent.doSelect(pButton, "@healer")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@healer rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@melee", 120, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_melee.blp", SynthiqBotsUI.tips.rtsc.melee, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@melee rtsc select")
	pButton.parent.doSelect(pButton, "@melee")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@melee rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@ranged", 150, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_ranged.blp", SynthiqBotsUI.tips.rtsc.ranged, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm").setDisable()
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("@ranged rtsc select")
	pButton.parent.doSelect(pButton, "@ranged")
	pButton.setEnable()
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("@ranged rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("@all", 180, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc.blp", SynthiqBotsUI.tips.rtsc.all, "SecureActionButtonTemplate").addMacro("type1", "/cast aedm")
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc select")
	pButton.parent.doReset(pButton.parent)
end
tButton.doLeft = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc select")
	pButton.parent.doReset(pButton.parent)
end

local tButton = tSelector.addButton("Browse", 210, 0, "Interface\\AddOns\\synthiqbots-ui\\Icons\\rtsc_browse.blp", SynthiqBotsUI.tips.rtsc.browse)
tButton.doRight = function(pButton)
	SynthiqBotsUI.ActionToGroup("rtsc cancel")
	pButton.parent.doReset(pButton.parent)
end
tButton.doLeft = function(pButton)
	local tFrame = pButton.parent
	
	if(pButton.state) then
		tFrame.buttons["@dps"].doShow()
		tFrame.buttons["@tank"].doShow()
		tFrame.buttons["@melee"].doShow()
		tFrame.buttons["@healer"].doShow()
		tFrame.buttons["@ranged"].doShow()
		tFrame.buttons["@group1"].doHide()
		tFrame.buttons["@group2"].doHide()
		tFrame.buttons["@group3"].doHide()
		tFrame.buttons["@group4"].doHide()
		tFrame.buttons["@group5"].doHide()
		pButton.state = false
	else
		tFrame.buttons["@dps"].doHide()
		tFrame.buttons["@tank"].doHide()
		tFrame.buttons["@healer"].doHide()
		tFrame.buttons["@melee"].doHide()
		tFrame.buttons["@ranged"].doHide()
		tFrame.buttons["@group1"].doShow()
		tFrame.buttons["@group2"].doShow()
		tFrame.buttons["@group3"].doShow()
		tFrame.buttons["@group4"].doShow()
		tFrame.buttons["@group5"].doShow()
		pButton.state = true
	end
end

-- FINISH --

SynthiqBotsUI.state = true
print("SynthiqBotsUI")