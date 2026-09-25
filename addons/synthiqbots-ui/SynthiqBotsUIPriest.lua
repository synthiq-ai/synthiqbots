SynthiqBotsUI.addPriest = function(pFrame, pCombat, pNormal)
	pFrame.addButton("Heal", 0, 0, "spell_holy_aspiration", SynthiqBotsUI.tips.priest.heal).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +heal,?", "co -heal,?", pButton.getName())) then
			pButton.getButton("Shadow").setDisable()
			pButton.getButton("Dps").setDisable()
		end
	end
	
	-- BUFF --
	
	pFrame.addButton("Buff", -30, 0, "spell_holy_power", SynthiqBotsUI.tips.priest.buff).setDisable()
	.doLeft = function(pButton)
		SynthiqBotsUI.OnOffActionToTarget(pButton, "co +buff,?", "co -buff,?", pButton.getName())
	end
	
	-- PLAYBOOK --
	
	pFrame.addButton("Playbook", -60, 0, "inv_misc_book_06", SynthiqBotsUI.tips.priest.playbook.master)
	.doLeft = function(pButton)
		SynthiqBotsUI.ShowHideSwitch(pButton.getFrame("Playbook"))
	end
	
	local tFrame = pFrame.addFrame("Playbook", -62, 30)
	tFrame:Hide()
	
	tFrame.addButton("ShadowDebuff", 0, 0, "spell_shadow_demonicempathy", SynthiqBotsUI.tips.priest.playbook.shadowDebuff).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +shadow debuff,?", "co -shadow debuff,?", pButton.getName())) then
			pButton.getButton("DpsDebuff").setEnable()
		else
			pButton.getButton("DpsDebuff").setDisable()
		end
	end
	
	tFrame.addButton("ShadowAoe", 0, 26, "spell_arcane_arcanetorrent", SynthiqBotsUI.tips.priest.playbook.shadowAoe).setDisable()
	.doLeft = function(pButton)
		SynthiqBotsUI.OnOffActionToTarget(pButton, "co +shadow aoe,?", "co -shadow aoe,?", pButton.getName())
	end
	
	tFrame.addButton("Shadow", 0, 52, "spell_holy_devotion", SynthiqBotsUI.tips.priest.playbook.shadow).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +shadow,?", "co -shadow,?", pButton.getName())) then
			pButton.getButton("Heal").setDisable()
			pButton.getButton("Dps").setEnable()
		else
			pButton.getButton("Dps").setDisable()
		end
	end
	
	-- DPS --
	
	pFrame.addButton("DpsControl", -90, 0, "ability_warrior_challange", SynthiqBotsUI.tips.priest.dps.master)
	.doLeft = function(pButton)
		SynthiqBotsUI.ShowHideSwitch(pButton.getFrame("DpsControl"))
	end
	
	local tFrame = pFrame.addFrame("DpsControl", -92, 30)
	tFrame:Hide()
	
	tFrame.addButton("DpsAssist", 0, 0, "spell_holy_heroism", SynthiqBotsUI.tips.priest.dps.dpsAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +healer dps,?", "co -healer dps,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	tFrame.addButton("DpsDebuff", 0, 26, "spell_holy_restoration", SynthiqBotsUI.tips.priest.dps.dpsDebuff).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +shadow debuff,?", "co -shadow debuff,?", pButton.getName())) then
			pButton.getButton("ShadowDebuff").setEnable()
		else
			pButton.getButton("ShadowDebuff").setDisable()
		end
	end
	
	tFrame.addButton("DpsAoe", 0, 52, "spell_holy_surgeoflight", SynthiqBotsUI.tips.priest.dps.dpsAoe).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +dps aoe,?", "co -dps aoe,?", pButton.getName())) then
			pButton.getButton("TankAssist").setDisable()
			pButton.getButton("DpsAssist").setDisable()
		end
	end
	
	tFrame.addButton("Dps", 0, 78, "spell_holy_divinepurpose", SynthiqBotsUI.tips.priest.dps.dps).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +shadow,?", "co -shadow,?", pButton.getName())) then
			pButton.getButton("Shadow").setEnable()
			pButton.getButton("Heal").setDisable()
		else
			pButton.getButton("Shadow").setDisable()
		end
	end
	
	-- ASSIST --
	
	pFrame.addButton("TankAssist", -120, 0, "ability_warrior_innerrage", SynthiqBotsUI.tips.priest.tankAssist).setDisable()
	.doLeft = function(pButton)
		if(SynthiqBotsUI.OnOffActionToTarget(pButton, "co +tank assist,?", "co -tank assist,?", pButton.getName())) then
			pButton.getButton("DpsAssist").setDisable()
			pButton.getButton("DpsAoe").setDisable()
		end
	end
	
	-- STRATEGIES --
	
	if(SynthiqBotsUI.isInside(pCombat, "heal")) then pFrame.getButton("Heal").setEnable() end
	if(SynthiqBotsUI.isInside(pNormal, "buff,")) then pFrame.getButton("Buff").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "shadow debuff")) then pFrame.getButton("ShadowDebuff").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "shadow aoe")) then pFrame.getButton("ShadowAoe").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "shadow,")) then pFrame.getButton("Shadow").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "healer dps")) then pFrame.getButton("DpsAssist").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "shadow debuff")) then pFrame.getButton("DpsDebuff").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "dps aoe")) then pFrame.getButton("DpsAoe").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "shadow,")) then pFrame.getButton("Shadow").setEnable() end
	if(SynthiqBotsUI.isInside(pCombat, "tank assist")) then pFrame.getButton("TankAssist").setEnable() end
end